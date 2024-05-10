package main

import (
	_ "embed"
	"fmt"
	"net/http"
)

//go:embed data/10G.gzip
var wrongMaxIntBeacon []byte

func sendMaxInt() func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(wrongMaxIntBeacon)))
		w.Write(wrongMaxIntBeacon)
	}
}
