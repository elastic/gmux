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
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/net/http2"
)

func TestBackendInitialSettingsIncludesNoRFC7540PrioritiesOnDefaultServer(t *testing.T) {
	settings, err := backendInitialSettings(new(http2.Server))
	require.NoError(t, err)
	require.NotEmpty(t, settings)

	var found bool
	for _, s := range settings {
		if s.ID == 0x9 && s.Val == 1 {
			found = true
			break
		}
	}
	require.True(t, found, "expected backend initial SETTINGS to include NO_RFC7540_PRIORITIES=1")
}

func TestGetConnHandlerMirrorsCustomBackendInitialSettings(t *testing.T) {
	defaultSettings, err := backendInitialSettings(new(http2.Server))
	require.NoError(t, err)

	conf := &http2.Server{
		MaxConcurrentStreams: 7,
		MaxReadFrameSize:     1 << 20,
	}
	customSettings, err := backendInitialSettings(conf)
	require.NoError(t, err)
	require.NotEqual(t, defaultSettings, customSettings, "custom server should emit different initial SETTINGS")
	require.Contains(t, customSettings, http2.Setting{
		ID:  http2.SettingMaxConcurrentStreams,
		Val: conf.MaxConcurrentStreams,
	})
	require.Contains(t, customSettings, http2.Setting{
		ID:  http2.SettingMaxFrameSize,
		Val: conf.MaxReadFrameSize,
	})

	testGetConnHandlerInitialSettings(t, customSettings)
}

func testGetConnHandlerInitialSettings(t *testing.T, initialSettings []http2.Setting) {
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

	var gotSettings []http2.Setting
	settingsFrame.ForeachSetting(func(s http2.Setting) error {
		gotSettings = append(gotSettings, s)
		return nil
	})
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
