package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"

	"github.com/darren/gpac"
)

var pacfile = flag.String("p", "wpad.dat", "pac file to load")
var addr = flag.String("l", "127.0.0.1:8080", "Listening address")

// Server the proxy server
type Server struct {
	http.Server
	pac *gpac.Parser
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

func (s *Server) handleConnect(w http.ResponseWriter, r *http.Request) {
	host, _, _ := net.SplitHostPort(r.Host)
	url := fmt.Sprintf("https://%s/", host)

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

	if proxy.IsDirect() {
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
	}

	src = combine(buf, src)

	go transfer(dst, src)
	go transfer(src, dst)

	log.Printf("[%s] %s %v [%v]", r.RemoteAddr, r.Method, url, proxy)
}

func transfer(destination io.WriteCloser, source io.ReadCloser) {
	defer destination.Close()
	defer source.Close()
	io.Copy(destination, source)
}

func (s *Server) handleHTTP(w http.ResponseWriter, req *http.Request) {
	proxies, err := s.pac.FindProxy(req.URL.String())
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}

	req.RequestURI = ""

	for _, proxy := range proxies {
		resp, err := proxy.Do(req)
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

	log.Printf("[%s] %s %v FAILED", req.RemoteAddr, req.Method, req.URL)
	http.Error(w, "No Proxy Available", http.StatusServiceUnavailable)
}

func cloneHeader(dst, src http.Header) {
	for k, vv := range src {
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

func main() {
	flag.Parse()

	log.Printf("Loading pac from %s", *pacfile)
	pac, err := gpac.From(*pacfile)
	if err != nil {
		log.Fatal(err)
	}

	log.Printf("Start proxy on %s", *addr)
	server := Server{
		Server: http.Server{
			Addr: *addr,
		},
		pac: pac,
	}

	server.Handler = http.HandlerFunc(server.handle)
	log.Fatal(server.ListenAndServe())
}
