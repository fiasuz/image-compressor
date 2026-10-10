package main

import (
	"bytes"
	"crypto/rand"
	"image"
	"image/color"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
)

func flatPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: uint8(x / 16 * 40), G: 90, B: 200, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func upload(t *testing.T, fields map[string]string, file []byte) *http.Request {
	t.Helper()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	for name, value := range fields {
		form.WriteField(name, value)
	}
	if file != nil {
		part, err := form.CreateFormFile("image", "in.png")
		if err != nil {
			t.Fatal(err)
		}
		part.Write(file)
	}
	form.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/compress", &body)
	req.Header.Set("Content-Type", form.FormDataContentType())
	return req
}

func TestCompress(t *testing.T) {
	input := flatPNG(t, 64, 64)
	handler := newServer("*", 1<<20, 1_000_000, 1).routes()
	for _, tc := range []struct {
		name   string
		fields map[string]string
		file   []byte
		want   int
	}{
		{"smart by default", nil, input, http.StatusOK},
		{"lossless", map[string]string{"mode": "lossless"}, input, http.StatusOK},
		{"lossy", map[string]string{"mode": "lossy", "colors": "16", "dither": "true", "quality": "70"}, input, http.StatusOK},
		{"unknown mode", map[string]string{"mode": "fast"}, input, http.StatusBadRequest},
		{"colors out of range", map[string]string{"mode": "lossy", "colors": "1"}, input, http.StatusBadRequest},
		{"psnr not a number", map[string]string{"min_psnr": "NaN"}, input, http.StatusBadRequest},
		{"missing file", nil, nil, http.StatusBadRequest},
		{"not an image", nil, []byte("hello"), http.StatusUnsupportedMediaType},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, upload(t, tc.fields, tc.file))
			if rec.Code != tc.want {
				t.Fatalf("status %d, want %d: %s", rec.Code, tc.want, rec.Body)
			}
			if tc.want != http.StatusOK {
				return
			}
			if got := rec.Header().Get("Content-Type"); got != "image/png" {
				t.Fatalf("Content-Type %q, want image/png", got)
			}
			if rec.Body.Len() > len(input) {
				t.Fatalf("output grew: %d -> %d bytes", len(input), rec.Body.Len())
			}
			if _, err := png.Decode(rec.Body); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUploadLimits(t *testing.T) {
	handler := newServer("*", 4<<10, 1_000, 1).routes()
	noise := make([]byte, 2<<20)
	rand.Read(noise)
	chunked := upload(t, nil, noise)
	chunked.ContentLength = -1
	for name, req := range map[string]*http.Request{
		"file over max-mb": upload(t, nil, make([]byte, 8<<10)),
		"body over limit":  upload(t, nil, noise),
		"chunked body":     chunked,
		"too many pixels":  upload(t, nil, flatPNG(t, 64, 64)),
	} {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("status %d, want 413: %s", rec.Code, rec.Body)
			}
		})
	}
}

func TestCORS(t *testing.T) {
	handler := newServer("https://app.example, https://www.app.example/", 1<<20, 1_000_000, 1).routes()
	for _, tc := range []struct {
		origin, wantHeader string
		want               int
	}{
		{"https://app.example", "https://app.example", http.StatusOK},
		{"https://www.app.example", "https://www.app.example", http.StatusOK},
		{"https://evil.example", "", http.StatusForbidden},
		{"", "", http.StatusOK},
	} {
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		if tc.origin != "" {
			req.Header.Set("Origin", tc.origin)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != tc.want || rec.Header().Get("Access-Control-Allow-Origin") != tc.wantHeader {
			t.Errorf("origin %q: status %d, allow %q", tc.origin, rec.Code, rec.Header().Get("Access-Control-Allow-Origin"))
		}
	}
}
