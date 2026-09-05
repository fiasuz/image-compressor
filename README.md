# Go Image Compressor

Compress PNG and JPEG images from the command line or a Go package, using only
the Go standard library. GIF files pass through unchanged, including animation.

[Contributing](CONTRIBUTING.md) · [Security](SECURITY.md) · [MIT license](LICENSE)

## Quick start

Requires **Go 1.22 or newer**. From a downloaded or cloned checkout:

```sh
go build -o bin/imgcompress ./cmd/imgcompress
./bin/imgcompress -in image.png -out image.min.png
./bin/imgcompress -in photo.jpg -out photo.min.jpg
```

On Windows, build with `-o bin/imgcompress.exe` and run `./bin/imgcompress.exe`.
For local installation into your Go binary directory, use
`go install ./cmd/imgcompress`.

The output path must not exist; create its parent directory first. Input format is
detected from file contents and retained, regardless of the output extension.
Use a matching extension. Successful runs print input/output byte counts and
percentage saved. Errors are written to stderr with a nonzero exit status.

## Compression modes

| Mode                | PNG                                                         | JPEG                                       | GIF            |
| ------------------- | ----------------------------------------------------------- | ------------------------------------------ | -------------- |
| Smart (CLI default) | Tests palette sizes from 16 to 256 colors                   | Tests encoder quality values from 40 to 95 | Original bytes |
| `-lossless`         | Re-encodes; uses an exact palette for eligible 8-bit images | Original bytes                             | Original bytes |
| `-lossy`            | Manual palette reduction, optional dithering                | Manual JPEG quality                        | Original bytes |

Every mode retains the original bytes if a candidate is not smaller. Savings are
not guaranteed. Lossless PNG preserves decoded pixels, including 16-bit channels,
but does not guarantee identical file bytes or metadata.

Smart mode selects the smallest tested candidate meeting the PSNR threshold
(default **37 dB**), or retains the original. PSNR is calculated on 8-bit RGBA
samples against the decoded input; it is not a perceptual quality guarantee.
Smart and manual lossy modes can change colors, transparency, and bit depth.

```sh
# Stricter smart quality threshold
./bin/imgcompress -in photo.jpg -out photo.min.jpg -min-psnr 40

# Preserve every decoded PNG pixel
./bin/imgcompress -in logo.png -out logo.min.png -lossless

# Reduce PNG palette with dithering
./bin/imgcompress -in image.png -out image.min.png -lossy -colors 128 -dither

# Set JPEG encoder quality manually
./bin/imgcompress -in photo.jpg -out photo.min.jpg -lossy -quality 75
```

## Compression results

Measured with Go 1.25.5 using deterministic, generated images from
[`examples/results`](examples/results/main.go). Sizes are exact byte counts.
These small synthetic examples illustrate behavior, not typical savings on
real-world photos or a comparison with other compressors.

| Synthetic input                       | Mode             | Before (bytes) | After (bytes) | Saved |
| ------------------------------------- | ---------------- | -------------: | ------------: | ----: |
| Three-color graphic, 128 × 128 PNG    | Lossless         |            457 |           162 | 64.6% |
| Gradient, 128 × 96 PNG                | Smart, 37 dB     |            285 |           285 |  0.0% |
| Gradient, 128 × 96 JPEG (quality 100) | Smart, 37 dB     |           4477 |          1121 | 75.0% |
| Seeded noise, 64 × 64 PNG             | Lossless         |          12425 |         12425 |  0.0% |
| Seeded noise, 64 × 64 PNG             | Lossy, 32 colors |          12425 |          2807 | 77.4% |
| Two-frame animation, 16 × 16 GIF      | Pass-through     |            116 |           116 |  0.0% |

The flat graphic shrinks without changing pixels. The already compact PNG
gradient and lossless noise retain their original bytes. Manual 32-color
quantization sacrifices color fidelity; its savings are not equivalent to
lossless compression. GIF animation is copied byte for byte.

Reproduce the table from the repository root:

```sh
go run ./examples/results
```

The example uses the same `compressor.Compress` function as the CLI and generates
all inputs in memory. PNG inputs use Go's default encoder; the JPEG input uses
quality 100. Encoder changes between Go versions may affect the numbers.

## Flags

| Flag        | Default  | Meaning                                             |
| ----------- | -------- | --------------------------------------------------- |
| `-in`       | required | Input PNG, JPEG, or GIF path                        |
| `-out`      | required | New output file path                                |
| `-lossless` | `false`  | Preserve decoded pixels; incompatible with `-lossy` |
| `-lossy`    | `false`  | Select manual lossy compression                     |
| `-colors`   | `256`    | Manual PNG palette limit, 2–256                     |
| `-dither`   | `false`  | Floyd–Steinberg dithering for manual PNG mode       |
| `-quality`  | `80`     | Manual JPEG quality, 1–100                          |
| `-min-psnr` | `37`     | Smart threshold, 20–60 dB                           |
| `-h`        |          | Display help                                        |

Dithering may improve gradients but increase file size. Format-specific options
are ignored when their mode or format does not apply. Package-level zero values
for colors, quality, and PSNR use 256, 80, and 37 respectively.

## Go package

This example runs inside the current module checkout. The module is currently
named `imagecompressor`; see [publishing notes](CONTRIBUTING.md#publishing-the-repository)
for configuring a public import path.

```go
package main

import (
    "bytes"
    "log"
    "os"

    "imagecompressor/compressor"
)

func main() {
    input, err := os.ReadFile("image.png")
    if err != nil {
        log.Fatal(err)
    }
    var output bytes.Buffer
    err = compressor.Compress(&output, bytes.NewReader(input), compressor.Options{
        Lossless: true,
    })
    if err != nil {
        log.Fatal(err)
    }
    log.Printf("%d -> %d bytes", len(input), output.Len())
}
```

`Compress` detects the input format; `CompressPNG` accepts PNG only. `Quantize`
returns a paletted image. Unlike the CLI, `Options{}` selects **manual lossy**
compression. Use `Options{Smart: true}` for automatic quality selection or
`Options{Lossless: true}` for pixel preservation. Set only one of these modes.

## Limitations

- WebP, AVIF, format conversion, resizing, and batch processing are not supported.
- Re-encoding does not preserve metadata such as EXIF or ICC profiles. Original
  bytes retained by the fallback keep their metadata.
- Animated PNG is not preserved when re-encoded; use static PNG inputs.
- Smart mode performs multiple compression attempts and can be slow on large images.
- Inputs and decoded images are held in memory with no built-in resource limits.

## Development

```sh
go test -race ./...
go vet ./...
go build ./cmd/imgcompress
```

GitHub Actions checks formatting, tests, and builds on Linux, macOS, and Windows
with Go 1.22 and the stable Go release. See [CONTRIBUTING.md](CONTRIBUTING.md)
for the development workflow. Licensed under [MIT](LICENSE).
