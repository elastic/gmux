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
	"golang.org/x/net/http2/hpack"
)

func TestGetConnHandlerWritesNoRFC7540PrioritiesSetting(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()

	m := &mux{
		http2Server:  new(http2.Server),
		grpcListener: newChanListener(),
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

	var haveNoRFC7540Priorities bool
	settingsFrame.ForeachSetting(func(s http2.Setting) error {
		if s.ID == noRFC7540PrioritiesSettingID && s.Val == 1 {
			haveNoRFC7540Priorities = true
		}
		return nil
	})
	require.True(t, haveNoRFC7540Priorities, "expected initial SETTINGS_NO_RFC7540_PRIORITIES=1")

	serverAckedClientSettings := make(chan struct{})
	readErr := make(chan error, 1)
	go func() {
		for {
			f, err := framer.ReadFrame()
			if err != nil {
				readErr <- err
				return
			}
			if sf, ok := f.(*http2.SettingsFrame); ok && sf.IsAck() {
				close(serverAckedClientSettings)
				return
			}
		}
	}()

	_, err = clientConn.Write([]byte(http2.ClientPreface))
	require.NoError(t, err)
	require.NoError(t, framer.WriteSettings())
	select {
	case <-serverAckedClientSettings:
	case err := <-readErr:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for server SETTINGS ACK")
	}
	require.NoError(t, framer.WriteSettingsAck())

	var headers bytes.Buffer
	encoder := hpack.NewEncoder(&headers)
	require.NoError(t, encoder.WriteField(hpack.HeaderField{Name: ":method", Value: "GET"}))
	require.NoError(t, encoder.WriteField(hpack.HeaderField{Name: ":scheme", Value: "https"}))
	require.NoError(t, encoder.WriteField(hpack.HeaderField{Name: ":authority", Value: "example.com"}))
	require.NoError(t, encoder.WriteField(hpack.HeaderField{Name: ":path", Value: "/"}))
	require.NoError(t, encoder.WriteField(hpack.HeaderField{Name: "content-type", Value: "application/grpc"}))
	require.NoError(t, framer.WriteHeaders(http2.HeadersFrameParam{
		StreamID:      1,
		BlockFragment: headers.Bytes(),
		EndHeaders:    true,
		EndStream:     true,
	}))

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for getConnHandler")
	}
}
