package modules

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/evilsocket/bettercap-ng/firewall"
	"github.com/evilsocket/bettercap-ng/log"
	"github.com/evilsocket/bettercap-ng/session"

	"github.com/elazarl/goproxy"
)

type HTTPProxy struct {
	Address     string
	Server      http.Server
	Redirection *firewall.Redirection
	Proxy       *goproxy.ProxyHttpServer
	Script      *ProxyScript
	CertFile    string
	KeyFile     string

	isTLS bool
	sess  *session.Session
}

func NewHTTPProxy(s *session.Session) *HTTPProxy {
	p := &HTTPProxy{
		Proxy: goproxy.NewProxyHttpServer(),
		sess:  s,
		isTLS: false,
	}

	p.Proxy.NonproxyHandler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if p.doProxy(req) == true {
			req.URL.Host = req.Host
			p.Proxy.ServeHTTP(w, req)
		}
	})

	p.Proxy.OnRequest().HandleConnect(goproxy.AlwaysMitm)
	p.Proxy.OnRequest().DoFunc(func(req *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
		if p.Script != nil {
			jsres := p.Script.OnRequest(req)
			if jsres != nil {
				p.logAction(req, jsres)
				return req, jsres.ToResponse(req)
			}
		}
		return req, nil
	})

	p.Proxy.OnResponse().DoFunc(func(res *http.Response, ctx *goproxy.ProxyCtx) *http.Response {
		if p.Script != nil {
			jsres := p.Script.OnResponse(res)
			if jsres != nil {
				p.logAction(res.Request, jsres)
				return jsres.ToResponse(res.Request)
			}
		}
		return res
	})

	return p
}

func (p *HTTPProxy) logAction(req *http.Request, jsres *JSResponse) {
	p.sess.Events.Add("http.proxy.spoofed-response", struct {
		To     string
		Method string
		Host   string
		Path   string
		Size   int
	}{
		strings.Split(req.RemoteAddr, ":")[0],
		req.Method,
		req.Host,
		req.URL.Path,
		len(jsres.Body),
	})
}

func (p *HTTPProxy) doProxy(req *http.Request) bool {
	blacklist := []string{
		"localhost",
		"127.0.0.1",
	}

	if req.Host == "" {
		log.Error("Got request with empty host: %v", req)
		return false
	}

	for _, blacklisted := range blacklist {
		if strings.HasPrefix(req.Host, blacklisted) {
			log.Error("Got request with blacklisted host: %s", req.Host)
			return false
		}
	}

	return true
}

func (p *HTTPProxy) Configure(address string, proxyPort int, httpPort int, scriptPath string) error {
	var err error

	p.Address = address

	if scriptPath != "" {
		if err, p.Script = LoadProxyScript(scriptPath, p.sess); err != nil {
			return err
		} else {
			log.Debug("Proxy script %s loaded.", scriptPath)
		}
	}

	p.Server = http.Server{
		Addr:    fmt.Sprintf("%s:%d", p.Address, proxyPort),
		Handler: p.Proxy,
	}

	if p.sess.Firewall.IsForwardingEnabled() == false {
		log.Info("Enabling forwarding.")
		p.sess.Firewall.EnableForwarding(true)
	}

	p.Redirection = firewall.NewRedirection(p.sess.Interface.Name(),
		"TCP",
		httpPort,
		p.Address,
		proxyPort)

	if err := p.sess.Firewall.EnableRedirection(p.Redirection, true); err != nil {
		return err
	}

	log.Debug("Applied redirection %s", p.Redirection.String())

	return nil
}

func (p *HTTPProxy) ConfigureTLS(address string, proxyPort int, httpPort int, scriptPath string, certFile string, keyFile string) error {
	err := p.Configure(address, proxyPort, httpPort, scriptPath)
	if err != nil {
		return err
	}

	p.isTLS = true
	p.CertFile = certFile
	p.KeyFile = keyFile

	return nil
}

func (p *HTTPProxy) Start() {
	go func() {
		var err error

		if p.isTLS == true {
			err = p.Server.ListenAndServeTLS(p.CertFile, p.KeyFile)
		} else {
			err = p.Server.ListenAndServe()
		}

		if err != nil {
			log.Warning("%s", err)
		}
	}()
}

func (p *HTTPProxy) Stop() error {
	if p.Redirection != nil {
		log.Debug("Disabling redirection %s", p.Redirection.String())
		if err := p.sess.Firewall.EnableRedirection(p.Redirection, false); err != nil {
			return err
		}
		p.Redirection = nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return p.Server.Shutdown(ctx)
}