// Command imgcompress-server exposes the compressor as an HTTP API.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"image"
	"io"
	"log"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"imagecompressor/compressor"
)

type server struct {
	origins   map[string]bool
	maxBytes  int64
	maxPixels int64
	slots     chan struct{}
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	addr := flag.String("addr", ":8080", "listen address")
	origins := flag.String("origin", "*", "allowed CORS origins, comma-separated, or *")
	maxMB := flag.Int("max-mb", 10, "maximum upload size in MB")
	maxMegapixels := flag.Int("max-megapixels", 16, "maximum image size in megapixels")
	workers := flag.Int("workers", runtime.NumCPU(), "maximum concurrent compressions")
	flag.Parse()
	if *maxMB < 1 || *maxMegapixels < 1 || *workers < 1 {
		return errors.New("-max-mb, -max-megapixels, and -workers must be positive")
	}

	s := newServer(*origins, int64(*maxMB)<<20, int64(*maxMegapixels)*1_000_000, *workers)
	srv := &http.Server{
		Addr:              *addr,
		Handler:           s.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       2 * time.Minute,
		// Smart mode can take about a minute for the largest photos on one core.
		WriteTimeout: 5 * time.Minute,
		IdleTimeout:  time.Minute,
	}
	log.Printf("listening on %s", *addr)
	return srv.ListenAndServe()
}

func newServer(origins string, maxBytes, maxPixels int64, workers int) *server {
	s := &server{
		origins:   make(map[string]bool),
		maxBytes:  maxBytes,
		maxPixels: maxPixels,
		slots:     make(chan struct{}, workers),
	}
	for _, origin := range strings.Split(origins, ",") {
		if origin = strings.TrimRight(strings.TrimSpace(origin), "/"); origin != "" {
			s.origins[origin] = true
		}
	}
	return s
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("GET /api/limits", s.limits)
	mux.HandleFunc("POST /api/compress", s.compress)
	return s.cors(mux)
}

func (s *server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if s.origins["*"] {
			w.Header().Set("Access-Control-Allow-Origin", "*")
		} else {
			w.Header().Add("Vary", "Origin")
			if origin != "" && !s.origins[origin] {
				http.Error(w, "origin not allowed", http.StatusForbidden)
				return
			}
			if origin != "" {
				w.Header().Set("Access-Control-Allow-Origin", origin)
			}
		}
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST")
			w.Header().Set("Access-Control-Max-Age", "86400")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *server) limits(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"maxBytes":%d,"maxPixels":%d}`+"\n", s.maxBytes, s.maxPixels)
}

func (s *server) compress(w http.ResponseWriter, r *http.Request) {
	// Room for the multipart envelope and the settings fields.
	limit := s.maxBytes + 1<<20
	if r.ContentLength > limit {
		http.Error(w, "file is too large", http.StatusRequestEntityTooLarge)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	// A memory limit equal to the body limit keeps uploads off the disk.
	if err := r.ParseMultipartForm(limit); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "file is too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "expected multipart form data", http.StatusBadRequest)
		return
	}
	file, header, err := r.FormFile("image")
	if err != nil {
		http.Error(w, "image file is required", http.StatusBadRequest)
		return
	}
	defer file.Close()
	if header.Size > s.maxBytes {
		http.Error(w, "file is too large", http.StatusRequestEntityTooLarge)
		return
	}
	opts, err := parseOptions(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	input, err := io.ReadAll(file)
	if err != nil {
		http.Error(w, "could not read upload", http.StatusBadRequest)
		return
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(input))
	if err != nil {
		http.Error(w, "unsupported or corrupt image; use PNG, JPEG, or GIF", http.StatusUnsupportedMediaType)
		return
	}
	if int64(cfg.Width)*int64(cfg.Height) > s.maxPixels {
		http.Error(w, "image dimensions are too large", http.StatusRequestEntityTooLarge)
		return
	}

	start := time.Now()
	var output bytes.Buffer
	err = s.withSlot(r.Context(), func() error {
		return compressor.Compress(&output, bytes.NewReader(input), opts)
	})
	if r.Context().Err() != nil {
		return
	}
	if err != nil {
		http.Error(w, "image could not be processed", http.StatusUnprocessableEntity)
		return
	}
	log.Printf("%s %dx%d mode=%s: %d -> %d bytes in %s", format, cfg.Width, cfg.Height,
		r.FormValue("mode"), len(input), output.Len(), time.Since(start).Round(time.Millisecond))

	w.Header().Set("Content-Type", "image/"+format)
	w.Header().Set("Content-Length", strconv.Itoa(output.Len()))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Write(output.Bytes())
}

func (s *server) withSlot(ctx context.Context, f func() error) error {
	select {
	case s.slots <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-s.slots }()
	return f()
}

func parseOptions(r *http.Request) (compressor.Options, error) {
	switch mode := r.FormValue("mode"); mode {
	case "", "smart":
		minPSNR, err := formValue(r, "min_psnr", 37, func(v string) (float64, error) {
			return strconv.ParseFloat(v, 64)
		})
		if err != nil || !(minPSNR >= 20 && minPSNR <= 60) {
			return compressor.Options{}, errors.New("min_psnr must be between 20 and 60")
		}
		return compressor.Options{Smart: true, MinPSNR: minPSNR}, nil
	case "lossless":
		return compressor.Options{Lossless: true}, nil
	case "lossy":
		colors, err := formValue(r, "colors", 256, strconv.Atoi)
		if err != nil || colors < 2 || colors > 256 {
			return compressor.Options{}, errors.New("colors must be between 2 and 256")
		}
		quality, err := formValue(r, "quality", 80, strconv.Atoi)
		if err != nil || quality < 1 || quality > 100 {
			return compressor.Options{}, errors.New("quality must be between 1 and 100")
		}
		dither, err := formValue(r, "dither", false, strconv.ParseBool)
		if err != nil {
			return compressor.Options{}, errors.New("dither must be true or false")
		}
		return compressor.Options{Colors: colors, Dither: dither, JPEGQuality: quality}, nil
	default:
		return compressor.Options{}, fmt.Errorf("unknown mode %q; use smart, lossless, or lossy", mode)
	}
}

func formValue[T any](r *http.Request, name string, fallback T, parse func(string) (T, error)) (T, error) {
	if v := r.FormValue(name); v != "" {
		return parse(v)
	}
	return fallback, nil
}
