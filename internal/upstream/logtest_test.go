package upstream

import (
	"bytes"
	"log"
	"testing"
)

// captureUpstreamLog 捕获 log 包输出（本包日志走标准 log），返回 fn 期间的全部文本。
func captureUpstreamLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	old := log.Writer()
	oldFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(old)
		log.SetFlags(oldFlags)
	})
	fn()
	log.SetOutput(old)
	log.SetFlags(oldFlags)
	return buf.String()
}
