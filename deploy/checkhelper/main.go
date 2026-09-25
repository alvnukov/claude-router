// checkhelper serves deploy/check.sh: it hands out free loopback ports and
// runs the slow event stream the check keeps open across a deploy.
//
//	checkhelper ports N   print N distinct free loopback ports
//	checkhelper upstream  print the address it listens on, then serve
package main

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

func main() {
	if len(os.Args) == 3 && os.Args[1] == "ports" {
		n, err := strconv.Atoi(os.Args[2])
		if err != nil {
			log.Fatal(err)
		}
		ports, err := freePorts(n)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println(strings.Join(ports, " "))
		return
	}
	if len(os.Args) == 2 && os.Args[1] == "upstream" {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println(listener.Addr().String())
		log.Fatal(http.Serve(listener, streamHandler()))
	}
	log.Fatal("usage: checkhelper ports N | checkhelper upstream")
}

// freePorts holds every listener until all are chosen, so the ports differ.
// The caller still has to check them again right before use.
func freePorts(n int) ([]string, error) {
	var ports []string
	for range n {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		defer listener.Close()
		ports = append(ports, strconv.Itoa(listener.Addr().(*net.TCPAddr).Port))
	}
	return ports, nil
}

// streamHandler answers /check/stream?events=N&every=D with N events, one
// every D, each flushed at once, then a final "done" event carrying N.
func streamHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /check/stream", func(w http.ResponseWriter, r *http.Request) {
		events, err := strconv.Atoi(r.URL.Query().Get("events"))
		if err != nil || events < 1 {
			http.Error(w, "events must be a positive number", http.StatusBadRequest)
			return
		}
		every, err := time.ParseDuration(r.URL.Query().Get("every"))
		if err != nil {
			http.Error(w, "every must be a duration", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		for i := range events {
			if i > 0 {
				select {
				case <-r.Context().Done():
					return
				case <-time.After(every):
				}
			}
			fmt.Fprintf(w, "data: %d\n\n", i)
			flusher.Flush()
		}
		fmt.Fprintf(w, "event: done\ndata: %d\n\n", events)
	})
	return mux
}
