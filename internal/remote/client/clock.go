package client

import "time"

func nowNano() int64 { return time.Now().UnixNano() }
