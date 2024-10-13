package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	forward "github.com/lrascao/udp-forward"
)

const (
	defaultTargetName string = "router2-node1"
	defaultTargetIP   string = "161.230.190.68"
	defaultTargetPort int    = 5321
)

type destination struct {
	Name string `json:"name"`
	Addr string `json:"addr"`
}

func main() {
	ctx := context.Background()

	logHandler := slog.NewTextHandler(os.Stdout,
		&slog.HandlerOptions{
			Level:     slog.LevelDebug,
			AddSource: true,
		})
	log := slog.New(logHandler)
	slog.SetDefault(log)

	updateDstCh := make(chan []destination)
	serveHTTP(ctx, updateDstCh)

	src := fmt.Sprintf("%s:%d", "0.0.0.0", 53)

	dst := fmt.Sprintf("%s:%d", defaultTargetIP, defaultTargetPort)

	forwarder, err := forward.NewForwarder(src,
		forward.WithDestination(defaultTargetName, dst),
		forward.WithTimeout(30*time.Second),
		forward.WithConnectCallback(func(addr string) {
			slog.Debug("connected", "from", addr)
		}),
		forward.WithDisconnectCallback(func(addr string) {
			slog.Debug("disconnected", "from", addr)
		}),
	)
	if err != nil {
		panic(err)
	}
	defer forwarder.Close()

	go func() {
		fmt.Printf("forwarding UDP on %s to %v\n",
			src, forwarder.Destinations())
		forwarder.Start(ctx)
	}()

	for {
		select {
		case <-ctx.Done():
		case destinations := <-updateDstCh:
			fmt.Printf("New target destinations: %v\n", destinations)
			var opts []forward.Option
			for _, dst := range destinations {
				opts = append(opts, forward.WithDestination(dst.Name, dst.Addr))
			}
			if err := forwarder.Update(opts...); err != nil {
				log.Error("Error updating forwarder", "error", err)
			}
			fmt.Printf("forwarding UDP on %s to %v\n",
				src, forwarder.Destinations())
		}
	}
}

func serveHTTP(ctx context.Context, ch chan []destination) {
	mux := http.NewServeMux()
	mux.HandleFunc("/",
		func(w http.ResponseWriter, r *http.Request) {
			// read the whole body into a string
			body, err := ioutil.ReadAll(r.Body)
			if err != nil {
				http.Error(w, "error reading body", http.StatusInternalServerError)
				return
			}
			var destinations []destination
			if err := json.Unmarshal(body, &destinations); err != nil {
				http.Error(w, "error parsing body", http.StatusBadRequest)
				return
			}

			ch <- destinations
		})

	// create an http server
	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", 5353),
		Handler: mux,
		BaseContext: func(l net.Listener) context.Context {
			return ctx
		},
	}

	go func() {
		err := srv.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			fmt.Printf("http server closed\n")
		} else if err != nil {
			fmt.Printf("error listening server: %v\n", err)
		}
	}()

}
