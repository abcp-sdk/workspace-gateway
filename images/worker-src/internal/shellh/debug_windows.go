//go:build windows

package shellh

import (
	"fmt"
	"os"
	"time"
)

func init() {
	// WORKER_TRACE=1 enables the exec/kill trace file (C:\Users\docker\
	// ewdebug.log path is fixed for the dockur VM; harmless elsewhere).
	if os.Getenv("WORKER_TRACE") == "1" {
		Debugf = func(format string, args ...interface{}) {
			f, err := os.OpenFile(os.Getenv("WORKER_TRACE_FILE"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
			if err != nil {
				return
			}
			defer f.Close()
			fmt.Fprintf(f, time.Now().Format("15:04:05.000")+" "+format+"\n", args...)
		}
	}
}
