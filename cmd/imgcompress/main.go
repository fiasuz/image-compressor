package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"imagecompressor/compressor"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	input := flag.String("in", "", "input PNG, JPG/JPEG, or GIF path")
	output := flag.String("out", "", "output path (keep the input extension)")
	lossless := flag.Bool("lossless", false, "preserve every pixel exactly")
	lossy := flag.Bool("lossy", false, "allow palette reduction (changes colors)")
	colors := flag.Int("colors", 256, "maximum palette colors (2-256)")
	dither := flag.Bool("dither", false, "enable dithering in lossy mode")
	jpegQuality := flag.Int("quality", 80, "JPEG quality in manual lossy mode (1-100)")
	minPSNR := flag.Float64("min-psnr", 37, "minimum smart-mode quality in dB (20-60)")
	flag.Parse()
	if *input == "" || *output == "" {
		fmt.Fprintln(os.Stderr, "usage: imgcompress -in input.png -out output.png [-min-psnr 37 | -lossless | -lossy -colors 128 -dither]")
		return errors.New("input and output paths are required")
	}
	if *lossless && *lossy {
		return errors.New("choose only one of -lossless or -lossy")
	}

	in, err := os.Open(*input)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(*output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		out.Close()
		if !ok {
			_ = os.Remove(*output)
		}
	}()

	if err := compressor.Compress(out, in, compressor.Options{
		Smart:       !*lossless && !*lossy,
		MinPSNR:     *minPSNR,
		Lossless:    *lossless,
		Colors:      *colors,
		Dither:      *dither,
		JPEGQuality: *jpegQuality,
	}); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	ok = true

	before, _ := os.Stat(*input)
	after, _ := os.Stat(*output)
	saving := 100 * (1 - float64(after.Size())/float64(before.Size()))
	fmt.Printf("%d -> %d bytes (%.1f%% saved)\n", before.Size(), after.Size(), saving)
	return nil
}
