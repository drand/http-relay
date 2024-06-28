package main

import (
	_ "embed"
	"net/http"
)

func sendMaxInt() func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=604800, immutable")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")

		w.Write([]byte("<html><head><title>Max Int issue</title></head><body>"))
		w.Write([]byte("<p>This is an invalid round value, you are trying to access round 18446744073709551615.</br>" +
			"But this is MaxInt64, you have a bug in your code causing an underflow.</br>" +
			"Please patch it instead of trying to get that round millions of time per hour.</p>"))
		w.Write([]byte("<p>这是一个无效的轮次值，你正尝试访问轮次号 18446744073709551615。</br>" +
			"但是这是 MaxInt64，你的代码中存在一个导致下溢的错误。</br>" +
			"请修复此错误，而不是每小时以数百万次的频率请求这个轮次，给我们的基础设施带来负担。</p>"))
		w.Write([]byte("</body></html>"))
	}
}
