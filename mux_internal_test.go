// Licensed to Elasticsearch B.V. under one or more contributor
// license agreements. See the NOTICE file distributed with
// this work for additional information regarding copyright
// ownership. Elasticsearch B.V. licenses this file to you under
// the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package gmux

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/net/http2"
)

func TestGetConnHandlerUsesBackendInitialSettings(t *testing.T) {
	defaultSettings, err := backendInitialSettings(new(http2.Server))
	require.NoError(t, err)

	conf := &http2.Server{
		MaxConcurrentStreams: 7,
		MaxReadFrameSize:     1 << 20,
	}
	customSettings, err := backendInitialSettings(conf)
	require.NoError(t, err)
	require.NotEmpty(t, customSettings)
	require.NotEqual(t, defaultSettings, customSettings, "custom server should emit different initial SETTINGS")
	require.Contains(t, customSettings, http2.Setting{
		ID:  http2.SettingMaxConcurrentStreams,
		Val: conf.MaxConcurrentStreams,
	})
	require.Contains(t, customSettings, http2.Setting{
		ID:  http2.SettingMaxFrameSize,
		Val: conf.MaxReadFrameSize,
	})

	assertGetConnHandlerInitialSettings(t, customSettings)
}

func TestHandleH2KeepsSettingsConsistentDuringRequest(t *testing.T) {
	conf := &http2.Server{
		MaxConcurrentStreams: 7,
		MaxReadFrameSize:     1 << 20,
	}
	initialSettings, err := backendInitialSettings(conf)
	require.NoError(t, err)
	require.NotEmpty(t, initialSettings)

	testServer := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "text/plain")
		_, _ = io.WriteString(w, "ok")
	}))
	_, err = ConfigureServer(testServer.Config, conf)
	require.NoError(t, err)
	testServer.EnableHTTP2 = true
	testServer.StartTLS()
	t.Cleanup(testServer.Close)

	baseTransport := testServer.Client().Transport.(*http.Transport)
	baseTLS := baseTransport.TLSClientConfig.Clone()
	baseTLS.NextProtos = []string{http2.NextProtoTLS}

	recordedConnCh := make(chan *recordingConn, 1)
	clientTransport := &http2.Transport{
		TLSClientConfig: baseTLS,
		DialTLSContext: func(_ context.Context, network, addr string, cfg *tls.Config) (net.Conn, error) {
			dialTLS := cfg
			if dialTLS == nil {
				dialTLS = baseTLS.Clone()
			}
			conn, err := tls.Dial(network, addr, dialTLS)
			if err != nil {
				return nil, err
			}
			recording := &recordingConn{Conn: conn}
			recordedConnCh <- recording
			return recording, nil
		},
	}
	t.Cleanup(clientTransport.CloseIdleConnections)

	client := &http.Client{Transport: clientTransport}
	resp, err := client.Get(testServer.URL)
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	clientTransport.CloseIdleConnections()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "ok", string(body))

	var recordedConn *recordingConn
	select {
	case recordedConn = <-recordedConnCh:
	default:
		t.Fatal("missing recorded client connection")
	}

	receivedSettings, err := collectNonACKSettings(recordedConn.Bytes())
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(receivedSettings), 2, "expected sniffing SETTINGS and backend SETTINGS")
	for _, settings := range receivedSettings {
		require.Equal(t, initialSettings, settings)
	}
}

func assertGetConnHandlerInitialSettings(t *testing.T, initialSettings []http2.Setting) {
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()

	m := &mux{
		http2Server:     new(http2.Server),
		grpcListener:    newChanListener(),
		initialSettings: append([]http2.Setting(nil), initialSettings...),
	}

	done := make(chan error, 1)
	go func() {
		_, err := m.getConnHandler(serverConn, new(bytes.Buffer))
		done <- err
	}()

	framer := http2.NewFramer(clientConn, clientConn)
	firstFrame, err := framer.ReadFrame()
	require.NoError(t, err)

	settingsFrame, ok := firstFrame.(*http2.SettingsFrame)
	require.True(t, ok, "expected first frame to be SETTINGS")

	gotSettings, err := settingsFromFrame(settingsFrame)
	require.NoError(t, err)
	require.Equal(t, initialSettings, gotSettings)

	// The test scope ends at initial SETTINGS content; close peer side to unblock
	// getConnHandler and avoid leaked goroutines.
	require.NoError(t, clientConn.Close())

	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for getConnHandler")
	}
}

type recordingConn struct {
	net.Conn
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *recordingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.mu.Lock()
		_, _ = c.buf.Write(p[:n])
		c.mu.Unlock()
	}
	return n, err
}

func (c *recordingConn) Bytes() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.buf.Bytes()...)
}

func collectNonACKSettings(payload []byte) ([][]http2.Setting, error) {
	framer := http2.NewFramer(nil, bytes.NewReader(payload))
	var settingsFrames [][]http2.Setting
	for {
		frame, err := framer.ReadFrame()
		switch {
		case err == nil:
		case errors.Is(err, io.EOF):
			return settingsFrames, nil
		default:
			return nil, err
		}

		settings, ok := frame.(*http2.SettingsFrame)
		if !ok || settings.IsAck() {
			continue
		}

		settingValues, err := settingsFromFrame(settings)
		if err != nil {
			return nil, err
		}
		settingsFrames = append(settingsFrames, settingValues)
	}
}

func settingsFromFrame(frame *http2.SettingsFrame) ([]http2.Setting, error) {
	var settings []http2.Setting
	if err := frame.ForeachSetting(func(s http2.Setting) error {
		settings = append(settings, s)
		return nil
	}); err != nil {
		return nil, err
	}
	return settings, nil
}
