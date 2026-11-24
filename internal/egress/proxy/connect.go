package proxy

import (
	"errors"
	"io"
	"net"
	"net/http"
	"sync/atomic"

	"github.com/jerryjuche/padi/internal/egress/config"
)

const connectEstablished = "HTTP/1.1 200 Connection Established\r\n\r\n"

func (p *Proxy) connect(w http.ResponseWriter, r *http.Request, info requestInfo) (int64, string, string) {
	host, port, err := ParseHostPort(r.Host)
	if err != nil {
		info.Host = safeField(r.Host, 253)
		p.recordDenial("build_proxy", info.Host, "bad_hostport", info)
		writeDeny(w, info, http.StatusForbidden, "bad_hostport")
		return 0, "deny", "bad_hostport"
	}
	info.Host = config.CanonicalHost(host)
	authority := net.JoinHostPort(host, port)
	if v := config.CheckURL("https://"+authority, p.cfg.URLLimits()); !v.Allow {
		p.recordDenial("build_proxy", info.Host, v.Reason, info)
		writeDeny(w, info, http.StatusForbidden, v.Reason)
		return 0, "deny", v.Reason
	}
	if !p.allow.Allowed(info.Host, info.TaskID, p.now()) {
		p.recordDenial("build_proxy", info.Host, "not_allowlisted", info)
		writeDeny(w, info, http.StatusForbidden, "not_allowlisted")
		return 0, "deny", "not_allowlisted"
	}
	if !p.limits.AllowConn(info.SessionID) {
		p.recordDenial("build_proxy", info.Host, "session_rate_limited", info)
		writeDeny(w, info, http.StatusTooManyRequests, "session_rate_limited")
		return 0, "deny", "session_rate_limited"
	}

	gd := p.guarded(info)
	checked, err := gd.CheckedAddresses(r.Context(), host, port)
	if err != nil {
		if errors.Is(err, config.ErrDenied) {
			reason := denyReason(err)
			writeDeny(w, info, http.StatusForbidden, reason)
			return 0, "deny", reason
		}
		writeProblem(w, http.StatusBadGateway, "upstream_error", "Could not resolve upstream host.")
		return 0, "deny", "dns_failed"
	}
	upstream, err := gd.DialChecked(r.Context(), "tcp", checked, port)
	if err != nil {
		writeProblem(w, http.StatusBadGateway, "upstream_error", "Could not reach upstream host.")
		return 0, "deny", "dial_failed"
	}
	defer upstream.Close()

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		writeProblem(w, http.StatusInternalServerError, "hijack_unsupported", "CONNECT is not supported by this server.")
		return 0, "deny", "hijack_unsupported"
	}
	client, bufrw, err := hijacker.Hijack()
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "hijack_failed", "Could not upgrade connection.")
		return 0, "deny", "hijack_failed"
	}
	defer client.Close()
	if _, err := io.WriteString(client, connectEstablished); err != nil {
		return 0, "deny", "hijack_failed"
	}

	var copied int64
	done := make(chan struct{}, 2)
	copyOne := func(dst io.Writer, src io.Reader) {
		n, _ := io.Copy(dst, src)
		atomic.AddInt64(&copied, n)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
		done <- struct{}{}
	}
	go copyOne(upstream, bufrw)
	go copyOne(client, upstream)
	<-done
	<-done
	return atomic.LoadInt64(&copied), "allow", ""
}
