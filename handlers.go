// Copyright 2013 The Gorilla Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

/*
Package handlers is a collection of handlers for use with Go's net/http package.
*/
package handlers

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/stoneedgetech/log"
)

func LogWrapper(handlerToWrap http.HandlerFunc, logger *log.Log) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		formatStr := "\n%v %v%v %v\nHost: %v\nUser-Agent: %v\nContent-Length: %v\n%v\n%v"
		var headerStr string
		for headerName, headerValueStringSlice := range r.Header {
			for _, headerValue := range headerValueStringSlice {
				headerStr += fmt.Sprintf("%v: %v", headerName, headerValue)
			}
		}
		bodyBytes, err := ioutil.ReadAll(r.Body)
		if err != nil {
			logger.Error("could not read request body")
		} else {
			var queryString string
			if r.URL.RawQuery != "" {
				queryString = "?" + r.URL.RawQuery
			}
			logger.Debug(fmt.Sprintf(formatStr, r.Method, r.URL.Path, queryString, r.Proto, r.Host, r.UserAgent(), r.ContentLength, headerStr, string(bodyBytes)))
		}

		r.Body = ioutil.NopCloser(bytes.NewBuffer(bodyBytes))
		handlerToWrap(w, r)
	}
}

// MethodHandler is an http.Handler that dispatches to a handler whose key in the MethodHandler's
// map matches the name of the HTTP request's method, eg: GET
//
// If the request's method is OPTIONS and OPTIONS is not a key in the map then the handler
// responds with a status of 200 and sets the Allow header to a comma-separated list of
// available methods.
//
// If the request's method doesn't match any of its keys the handler responds with
// a status of 406, Method not allowed and sets the Allow header to a comma-separated list
// of available methods.
type MethodHandler map[string]http.Handler

func (h MethodHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if handler, ok := h[req.Method]; ok {
		handler.ServeHTTP(w, req)
	} else {
		allow := []string{}
		for k := range h {
			allow = append(allow, k)
		}
		sort.Strings(allow)
		w.Header().Set("Allow", strings.Join(allow, ", "))
		if req.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
		} else {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	}
}

// loggingHandler is the http.Handler implementation for LoggingHandlerTo and its friends
type loggingHandler struct {
	writer  io.Writer
	handler http.Handler
}

// combinedLoggingHandler is the http.Handler implementation for LoggingHandlerTo and its friends
type combinedLoggingHandler struct {
	writer  io.Writer
	handler http.Handler
}

type forwardedLoggingHandler struct {
	writer  io.Writer
	handler http.Handler
}

type queueLoggingHandler struct {
	logger  *log.Log
	handler http.Handler
}

func (h loggingHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	t := time.Now()
	var logger loggingResponseWriter
	if _, ok := w.(http.Hijacker); ok {
		logger = &hijackLogger{responseLogger: responseLogger{w: w}}
	} else {
		logger = &responseLogger{w: w}
	}
	h.handler.ServeHTTP(logger, req)
	writeLog(h.writer, req, t, logger.Status(), logger.Size())
}

func (h combinedLoggingHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	t := time.Now()
	var logger loggingResponseWriter
	if _, ok := w.(http.Hijacker); ok {
		logger = &hijackLogger{responseLogger: responseLogger{w: w}}
	} else {
		logger = &responseLogger{w: w}
	}
	h.handler.ServeHTTP(logger, req)
	writeCombinedLog(h.writer, req, t, logger.Status(), logger.Size())
}

func (h forwardedLoggingHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	t := time.Now()
	var logger loggingResponseWriter
	if _, ok := w.(http.Hijacker); ok {
		logger = &hijackLogger{responseLogger: responseLogger{w: w}}
	} else {
		logger = &responseLogger{w: w}
	}
	h.handler.ServeHTTP(logger, req)
	writeForwardedLog(h.writer, req, t, logger.Status(), logger.Size())
}

func (h queueLoggingHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	t := time.Now()
	var logger loggingResponseWriter
	if _, ok := w.(http.Hijacker); ok {
		logger = &hijackLogger{responseLogger: responseLogger{w: w}}
	} else {
		logger = &responseLogger{w: w}
	}
	h.handler.ServeHTTP(logger, req)
	h.logger.Debug(string(buildCommonLogLine(req, t, logger.Status(), logger.Size())))
}

type loggingResponseWriter interface {
	http.ResponseWriter
	Status() int
	Size() int
}

// responseLogger is wrapper of http.ResponseWriter that keeps track of its HTTP status
// code and body size
type responseLogger struct {
	w      http.ResponseWriter
	status int
	size   int
}

func (l *responseLogger) Header() http.Header {
	return l.w.Header()
}

func (l *responseLogger) Write(b []byte) (int, error) {
	if l.status == 0 {
		// The status will be StatusOK if WriteHeader has not been called yet
		l.status = http.StatusOK
	}
	size, err := l.w.Write(b)
	l.size += size
	return size, err
}

func (l *responseLogger) WriteHeader(s int) {
	l.w.WriteHeader(s)
	l.status = s
}

func (l *responseLogger) Status() int {
	return l.status
}

func (l *responseLogger) Size() int {
	return l.size
}

type hijackLogger struct {
	responseLogger
}

func (l *hijackLogger) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h := l.responseLogger.w.(http.Hijacker)
	conn, rw, err := h.Hijack()
	if err == nil && l.responseLogger.status == 0 {
		// The status will be StatusSwitchingProtocols if there was no error and WriteHeader has not been called yet
		l.responseLogger.status = http.StatusSwitchingProtocols
	}
	return conn, rw, err
}

const lowerhex = "0123456789abcdef"

func appendQuoted(buf []byte, s string) []byte {
	var runeTmp [utf8.UTFMax]byte
	for width := 0; len(s) > 0; s = s[width:] {
		r := rune(s[0])
		width = 1
		if r >= utf8.RuneSelf {
			r, width = utf8.DecodeRuneInString(s)
		}
		if width == 1 && r == utf8.RuneError {
			buf = append(buf, `\x`...)
			buf = append(buf, lowerhex[s[0]>>4])
			buf = append(buf, lowerhex[s[0]&0xF])
			continue
		}
		if r == rune('"') || r == '\\' { // always backslashed
			buf = append(buf, '\\')
			buf = append(buf, byte(r))
			continue
		}
		if strconv.IsPrint(r) {
			n := utf8.EncodeRune(runeTmp[:], r)
			buf = append(buf, runeTmp[:n]...)
			continue
		}
		switch r {
		case '\a':
			buf = append(buf, `\a`...)
		case '\b':
			buf = append(buf, `\b`...)
		case '\f':
			buf = append(buf, `\f`...)
		case '\n':
			buf = append(buf, `\n`...)
		case '\r':
			buf = append(buf, `\r`...)
		case '\t':
			buf = append(buf, `\t`...)
		case '\v':
			buf = append(buf, `\v`...)
		default:
			switch {
			case r < ' ':
				buf = append(buf, `\x`...)
				buf = append(buf, lowerhex[s[0]>>4])
				buf = append(buf, lowerhex[s[0]&0xF])
			case r > utf8.MaxRune:
				r = 0xFFFD
				fallthrough
			case r < 0x10000:
				buf = append(buf, `\u`...)
				for s := 12; s >= 0; s -= 4 {
					buf = append(buf, lowerhex[r>>uint(s)&0xF])
				}
			default:
				buf = append(buf, `\U`...)
				for s := 28; s >= 0; s -= 4 {
					buf = append(buf, lowerhex[r>>uint(s)&0xF])
				}
			}
		}
	}
	return buf

}

// buildCommonLogLine builds a log entry for req in Apache Common Log Format.
// ts is the timestamp with which the entry should be logged.
// status and size are used to provide the response HTTP status and size.
func buildCommonLogLine(req *http.Request, ts time.Time, status int, size int) []byte {
	username := "-"
	if req.URL.User != nil {
		if name := req.URL.User.Username(); name != "" {
			username = name
		}
	}

	host, _, err := net.SplitHostPort(req.RemoteAddr)

	if err != nil {
		host = req.RemoteAddr
	}

	uri := req.URL.RequestURI()

	buf := make([]byte, 0, 3*(len(host)+len(username)+len(req.Method)+len(uri)+len(req.Proto)+50)/2)
	buf = append(buf, host...)
	buf = append(buf, " - "...)
	buf = append(buf, username...)
	buf = append(buf, " ["...)
	buf = append(buf, ts.Format("02/Jan/2006:15:04:05 -0700")...)
	buf = append(buf, `] "`...)
	buf = append(buf, req.Method...)
	buf = append(buf, " "...)
	buf = appendQuoted(buf, uri)
	buf = append(buf, " "...)
	buf = append(buf, req.Proto...)
	buf = append(buf, `" `...)
	buf = append(buf, strconv.Itoa(status)...)
	buf = append(buf, " "...)
	buf = append(buf, strconv.Itoa(size)...)
	return buf
}

// writeLog writes a log entry for req to w in Apache Common Log Format.
// ts is the timestamp with which the entry should be logged.
// status and size are used to provide the response HTTP status and size.
func writeLog(w io.Writer, req *http.Request, ts time.Time, status, size int) {
	buf := buildCommonLogLine(req, ts, status, size)
	buf = append(buf, '\n')
	w.Write(buf)
}

// writeCombinedLog writes a log entry for req to w in Apache Combined Log Format.
// ts is the timestamp with which the entry should be logged.
// status and size are used to provide the response HTTP status and size.
func writeCombinedLog(w io.Writer, req *http.Request, ts time.Time, status, size int) {
	buf := buildCommonLogLine(req, ts, status, size)
	buf = append(buf, ` "`...)
	buf = appendQuoted(buf, req.Referer())
	buf = append(buf, `" "`...)
	buf = appendQuoted(buf, req.UserAgent())
	buf = append(buf, '"', '\n')
	w.Write(buf)
}

func writeForwardedLog(w io.Writer, req *http.Request, ts time.Time, status, size int) {
	buf := buildCommonLogLine(req, ts, status, size)
	buf = append(buf, ' ')
	buf = append(buf, req.Header.Get("X-Forwarded-For")...)
	buf = append(buf, '\n')
	w.Write(buf)
}

// CombinedLoggingHandler return a http.Handler that wraps h and logs requests to out in
// Apache Combined Log Format.
//
// See http://httpd.apache.org/docs/2.2/logs.html#combined for a description of this format.
//
// LoggingHandler always sets the ident field of the log to -
func CombinedLoggingHandler(out io.Writer, h http.Handler) http.Handler {
	return combinedLoggingHandler{out, h}
}

// LoggingHandler return a http.Handler that wraps h and logs requests to out in
// Apache Common Log Format (CLF).
//
// See http://httpd.apache.org/docs/2.2/logs.html#common for a description of this format.
//
// LoggingHandler always sets the ident field of the log to -
func LoggingHandler(out io.Writer, h http.Handler) http.Handler {
	return loggingHandler{out, h}
}

func ForwardedLoggingHandler(out io.Writer, h http.Handler) http.Handler {
	return forwardedLoggingHandler{out, h}
}

func QueueLoggingHandler(log *log.Log, h http.Handler) http.Handler {
	return queueLoggingHandler{log, h}
}
