package failover

import (
	"bytes"
	"io"
	"strings"
)

// WriteSSEEvent 以标准 SSE 帧（event/id/多行 data/空行）写出单个事件，不负责 flush。
func WriteSSEEvent(w io.Writer, event SSEEvent) error {
	var buf bytes.Buffer
	if event.Event != "" {
		buf.WriteString("event: " + event.Event + "\n")
	}
	if event.ID != "" {
		buf.WriteString("id: " + event.ID + "\n")
	}
	if event.Data != "" {
		for _, line := range strings.Split(event.Data, "\n") {
			buf.WriteString("data: " + line + "\n")
		}
	}
	buf.WriteString("\n")
	_, err := w.Write(buf.Bytes())
	return err
}
