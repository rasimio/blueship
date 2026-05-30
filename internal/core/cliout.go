package core

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// --- CLI output helpers ---

// Response is the standard JSON output format for CLI commands.
type Response struct {
	Success  bool        `json:"success"`
	Data     interface{} `json:"data,omitempty"`
	Error    string      `json:"error,omitempty"`
	Fallback interface{} `json:"fallback,omitempty"`
	Cached   bool        `json:"cached"`
	CachedAt *time.Time  `json:"cached_at,omitempty"`
}

// OK writes a success response to stdout.
func OK(data interface{}) {
	r := Response{Success: true, Data: data, Cached: false}
	writeResponse(r)
}

// Fail writes an error response to stdout and exits.
func Fail(err string) {
	r := Response{Success: false, Error: err, Cached: false}
	writeResponse(r)
	os.Exit(1)
}

func writeResponse(r Response) {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "output marshal error: %v\n", err)
		fmt.Printf(`{"success":false,"error":"internal marshal error","cached":false}`)
		os.Exit(1)
	}
	fmt.Println(string(data))
}
