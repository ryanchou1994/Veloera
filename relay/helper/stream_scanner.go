// Copyright (c) 2025 Tethys Plex
//
// This file is part of Veloera.
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program. If not, see <https://www.gnu.org/licenses/>.
package helper

import (
	"bufio"
	"context"
	"github.com/bytedance/gopkg/util/gopool"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
	"veloera/common"
	"veloera/constant"
	relaycommon "veloera/relay/common"
	"veloera/setting/operation_setting"

	"github.com/gin-gonic/gin"
)

const (
	InitialScannerBufferSize = 1 << 20  // 1MB (1*1024*1024)
	MaxScannerBufferSize     = 10 << 20 // 10MB (10*1024*1024)
	DefaultPingInterval      = 10 * time.Second
)

func StreamScannerHandler(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo, dataHandler func(data string) bool) {

	if resp == nil || dataHandler == nil {
		return
	}

	defer resp.Body.Close()

	streamingTimeout := time.Duration(constant.StreamingTimeout) * time.Second
	if strings.HasPrefix(info.UpstreamModelName, "o1") || strings.HasPrefix(info.UpstreamModelName, "o3") {
		// twice timeout for thinking model
		streamingTimeout *= 2
	}

	var (
		stopChan   = make(chan bool, 2)
		scanner    = bufio.NewScanner(resp.Body)
		ticker     = time.NewTicker(streamingTimeout)
		pingTicker *time.Ticker
		writeMutex sync.Mutex // Mutex to protect concurrent writes
	)

	generalSettings := operation_setting.GetGeneralSetting()
	pingEnabled := generalSettings.PingIntervalEnabled
	pingInterval := time.Duration(generalSettings.PingIntervalSeconds) * time.Second
	if pingInterval <= 0 {
		pingInterval = DefaultPingInterval
	}

	if pingEnabled {
		pingTicker = time.NewTicker(pingInterval)
	}

	defer func() {
		ticker.Stop()
		if pingTicker != nil {
			pingTicker.Stop()
		}
		close(stopChan)
	}()
	scanner.Buffer(make([]byte, InitialScannerBufferSize), MaxScannerBufferSize)
	scanner.Split(bufio.ScanLines)
	SetEventStreamHeaders(c)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ctx = context.WithValue(ctx, "stop_chan", stopChan)

	// Handle ping data sending
	if pingEnabled && pingTicker != nil {
		gopool.Go(func() {
			for {
				select {
				case <-pingTicker.C:
					writeMutex.Lock() // Lock before writing
					err := PingData(c)
					writeMutex.Unlock() // Unlock after writing
					if err != nil {
						common.LogError(c, "ping data error: "+err.Error())
						common.SafeSendBool(stopChan, true)
						return
					}
					if common.DebugEnabled {
						println("ping data sent")
					}
				case <-ctx.Done():
					if common.DebugEnabled {
						println("ping data goroutine stopped")
					}
					return
				}
			}
		})
	}

	common.RelayCtxGo(ctx, func() {
		for scanner.Scan() {
			ticker.Reset(streamingTimeout)
			data := scanner.Text()
			if common.DebugEnabled {
				println(data)
			}

			if len(data) < 6 {
				continue
			}
			if data[:5] != "data:" && data[:6] != "[DONE]" {
				continue
			}
			data = data[5:]
			data = strings.TrimLeft(data, " ")
			data = strings.TrimSuffix(data, "\"")
			if !strings.HasPrefix(data, "[DONE]") {
				info.SetFirstResponseTime()
				writeMutex.Lock() // Lock before writing
				success := dataHandler(data)
				writeMutex.Unlock() // Unlock after writing
				if !success {
					break
				}
			}
		}

		if err := scanner.Err(); err != nil {
			if err != io.EOF {
				common.LogError(c, "scanner error: "+err.Error())
			}
		}

		common.SafeSendBool(stopChan, true)
	})

	select {
	case <-ticker.C:
		// 超时处理逻辑
		common.LogError(c, "streaming timeout")
		common.SafeSendBool(stopChan, true)
	case <-stopChan:
		// 正常结束
		common.LogInfo(c, "streaming finished")
	}
}
