package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/darren/gpac"
)

var pacfile = flag.String("p", "wpad.dat", "pac file to load")
var addr = flag.String("l", "127.0.0.1:8080", "Listening address")
var refresh = flag.Duration("r", 0, "Time duration to refresh pac file")

// Server the proxy server
type Server struct {
	http.Server
	sync.Mutex

	pacfile         string
	pac             *gpac.Parser
	refreshDuration time.Duration
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		s.handleConnect(w, r)
	} else {
		s.handleHTTP(w, r)
	}
}

type peekedConn struct {
	net.Conn
	r io.Reader
}

// concat combine conn and peeked buffer
func combine(peeked io.Reader, conn net.Conn) *peekedConn {
	r := io.MultiReader(peeked, conn)
	return &peekedConn{conn, r}
}

func (p *peekedConn) Read(data []byte) (int, error) {
	return p.r.Read(data)
}

// Copied from https://github.com/golang/go/blob/master/src/net/http/httputil/reverseproxy.go

// Hop-by-hop headers. These are removed when sent to the backend.
// http://www.w3.org/Protocols/rfc2616/rfc2616-sec13.html
var hopHeaders = []string{
	"Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te", // canonicalized version of "TE"
	"Trailers",
	"Transfer-Encoding",
	"Upgrade",
}

// removeConnectionHeaders removes hop-by-hop headers listed in the "Connection" header of h.
// See RFC 7230, section 6.1
func removeConnectionHeaders(h http.Header) {
	if c := h.Get("Connection"); c != "" {
		for _, f := range strings.Split(c, ",") {
			if f = strings.TrimSpace(f); f != "" {
				h.Del(f)
			}
		}
	}
}

func removeHopHeaders(h http.Header) {
	for _, k := range hopHeaders {
		hv := h.Get(k)
		if hv == "" {
			continue
		}
		if k == "Te" && hv == "trailers" {
			continue
		}
		h.Del(k)
	}
}

// prune clean http header
func prune(h http.Header) {
	removeConnectionHeaders(h)
	removeHopHeaders(h)
}

func (s *Server) handleConnect(w http.ResponseWriter, r *http.Request) {
	host, port, _ := net.SplitHostPort(r.Host)
	var url string

	if port == "443" {
		url = fmt.Sprintf("https://%s/", host)
	} else {
		url = fmt.Sprintf("https://%s:%s/", host, port)
	}

	proxies, err := s.pac.FindProxy(url)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}

	ctx := context.Background()

	var dst net.Conn
	var proxy *gpac.Proxy

	for _, proxy = range proxies {
		dialer := proxy.Dialer()
		dst, err = dialer(ctx, "tcp", r.Host)
		if err != nil {
			log.Println("Dial failed:", err)
			continue
		} else {
			break
		}
	}

	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}

	if proxy == nil {
		http.Error(w, "No Proxy Available", http.StatusServiceUnavailable)
		return
	}

	if proxy.IsDirect() || proxy.IsSOCKS() {
		w.WriteHeader(http.StatusOK)
	}

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "Hijacking not supported", http.StatusInternalServerError)
		return
	}

	src, buf, err := hijacker.Hijack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}

	src = combine(buf, src)

	go pipe(dst, src)
	go pipe(src, dst)

	log.Printf("[%s] %s %v [%v]", r.RemoteAddr, r.Method, url, proxy)
}

func pipe(destination io.WriteCloser, source io.ReadCloser) {
	defer destination.Close()
	defer source.Close()
	io.Copy(destination, source)
}

func (s *Server) handleHTTP(w http.ResponseWriter, req *http.Request) {
	var perr error

	proxies, err := s.pac.FindProxy(req.URL.String())
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}

	prune(req.Header)

	for _, proxy := range proxies {
		resp, err := proxy.Transport().RoundTrip(req)
		perr = err
		if err != nil {
			continue
		}

		defer resp.Body.Close()
		cloneHeader(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)

		log.Printf("[%s] %s %v [%v]", req.RemoteAddr, req.Method, req.URL, proxy)

		if err == nil {
			return
		}
	}

	log.Printf("[%s] %s %v FAILED: %v", req.RemoteAddr, req.Method, req.URL, perr)
	http.Error(w, "No Proxy Available", http.StatusServiceUnavailable)
}

func (s *Server) watch() {
	for {
		time.Sleep(s.refreshDuration)
		log.Printf("Try reloading from %s", s.pacfile)
		pac, err := gpac.From(s.pacfile)

		if pac.Source() == s.pac.Source() {
			log.Println("Pac file not changed")
			continue
		}

		if err != nil {
			log.Println("Refresh pac failed: %v", err)
		} else {
			log.Println("Refresh pac succeeded")
		}

		s.Lock()
		s.pac = pac
		s.Unlock()
	}
}

// Start starts the proxy server
func (s *Server) Start() error {
	log.Printf("Start proxy on %s", s.Server.Addr)
	if s.refreshDuration > 0 {
		log.Printf("Start pac file watcher on: %s, refresh time: %v", s.pacfile, s.refreshDuration)
		go s.watch()
	}
	s.Handler = http.HandlerFunc(s.handle)
	return s.ListenAndServe()
}

// New create the proxy server
func New(addr string, pacf string, rintval time.Duration) (*Server, error) {
	pac, err := gpac.From(pacf)
	if err != nil {
		return nil, err
	}

	return &Server{
		Server: http.Server{
			Addr: addr,
		},
		pac:             pac,
		pacfile:         pacf,
		refreshDuration: rintval,
	}, nil
}

func cloneHeader(dst, src http.Header) {
	for k, vv := range src {
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	flag.Parse()

	server, err := New(*addr, *pacfile, *refresh)
	if err != nil {
		log.Fatal(err)
	}

	log.Fatal(server.Start())
}
