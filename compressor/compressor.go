// Package compressor optimizes PNG and JPEG images and preserves GIF files.
package compressor

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	_ "image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"math"
	"runtime"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
)

// Options controls PNG and JPEG compression. The zero value selects manual
// lossy compression; set Smart or Lossless explicitly to use those modes.
type Options struct {
	// Smart tries several palette sizes and keeps the smallest result whose
	// pixel PSNR is at least MinPSNR. Candidates run on up to GOMAXPROCS
	// goroutines. Avoid enabling Smart and Lossless together.
	Smart bool
	// MinPSNR is the minimum quality for Smart mode. Zero uses 37 dB, a
	// default. PSNR is a pixel metric, not a guarantee of visual equivalence.
	MinPSNR float64
	// Lossless preserves every decoded pixel exactly. If recompression is not
	// smaller, CompressPNG writes the original file unchanged.
	Lossless bool
	// Colors is the maximum palette size. Valid values are 2 through 256.
	// Zero uses 256. It is used only for manual lossy PNG compression.
	Colors int
	// Dither applies Floyd-Steinberg error diffusion. It can improve gradients,
	// but may produce a slightly larger PNG.
	Dither bool
	// JPEGQuality sets quality in manual lossy JPEG mode. Zero uses 80.
	JPEGQuality int
}

type sample struct {
	r, g, b, a uint8
	count      int
}

type bucket struct {
	colors []sample
	weight int
	// channel and spread describe the widest channel, cached because bucket
	// selection reads them on every split.
	channel, spread int
}

// histogram holds the unique colors of an image, sorted by rgbaKey, and the
// index of every pixel's color, so palettes can be built, measured, and
// applied without rescanning the pixels.
type histogram struct {
	img     *image.NRGBA
	samples []sample
	pixels  []uint32
}

// paletteSearch finds nearest palette entries without a full scan. Entries
// are ordered by their projection onto (1, 1, 1, 2), which matches the
// distance weights: by Cauchy-Schwarz, (Δr+Δg+Δb+2Δa)² ≤ 5·distance, so once
// the projected gap alone exceeds the best distance, no further entry in that
// direction can win.
type paletteSearch struct {
	colors []color.NRGBA
	order  []uint8
	keys   []int
}

// CompressPNG optimizes a PNG. Lossless mode preserves every decoded pixel.
// The original bytes are returned when recompression would make the file larger.
func CompressPNG(dst io.Writer, src io.Reader, opts Options) error {
	original, err := io.ReadAll(src)
	if err != nil {
		return err
	}
	img, err := png.Decode(bytes.NewReader(original))
	if err != nil {
		return err
	}
	return compressPNGBytes(dst, original, img, opts)
}

// Compress detects and compresses PNG and JPEG input. GIF files are accepted
// but copied unchanged to preserve animation. The output keeps the input format.
func Compress(dst io.Writer, src io.Reader, opts Options) error {
	original, err := io.ReadAll(src)
	if err != nil {
		return err
	}
	img, format, err := image.Decode(bytes.NewReader(original))
	if err != nil {
		return err
	}
	switch format {
	case "png":
		return compressPNGBytes(dst, original, img, opts)
	case "jpeg":
		return compressJPEGBytes(dst, original, img, opts)
	case "gif":
		_, err = dst.Write(original)
		return err
	default:
		return fmt.Errorf("unsupported image format %q", format)
	}
}

func compressPNGBytes(dst io.Writer, original []byte, img image.Image, opts Options) error {

	if opts.Smart {
		return compressSmart(dst, original, img, opts)
	}

	var result image.Image = img
	if opts.Lossless {
		// An RGBA PNG with at most 256 exact colors can be represented as an
		// indexed PNG without changing a single pixel. This is often the biggest
		// lossless win for icons, logos, screenshots, and flat illustrations.
		if paletted, ok := exactPalette(img, 256); ok {
			result = paletted
		}
	} else {
		quantized, err := Quantize(img, opts)
		if err != nil {
			return err
		}
		result = quantized
	}

	encoded, err := encodePNG(result)
	if err != nil {
		return err
	}
	if len(encoded) >= len(original) {
		_, err = dst.Write(original)
		return err
	}
	_, err = dst.Write(encoded)
	return err
}

func compressJPEGBytes(dst io.Writer, original []byte, img image.Image, opts Options) error {
	if opts.Lossless {
		_, err := dst.Write(original)
		return err
	}
	if !opts.Smart {
		quality := opts.JPEGQuality
		if quality == 0 {
			quality = 80
		}
		if quality < 1 || quality > 100 {
			return errors.New("JPEG quality must be between 1 and 100")
		}
		encoded, err := encodeJPEG(img, quality)
		if err != nil {
			return err
		}
		if len(encoded) >= len(original) {
			encoded = original
		}
		_, err = dst.Write(encoded)
		return err
	}

	minPSNR := opts.MinPSNR
	if minPSNR == 0 {
		minPSNR = 37
	}
	if minPSNR < 20 || minPSNR > 60 {
		return errors.New("minimum PSNR must be between 20 and 60 dB")
	}
	reference := referencePixels(img)
	qualities := []int{95, 92, 90, 88, 85, 82, 80, 78, 75, 72, 70, 68, 65, 60, 55, 50, 45, 40}
	passing := make([][]byte, len(qualities))
	err := forEach(len(qualities), func(i int) error {
		encoded, err := encodeJPEG(img, qualities[i])
		if err != nil {
			return err
		}
		candidate, err := jpeg.Decode(bytes.NewReader(encoded))
		if err != nil {
			return err
		}
		if sameSize(img, candidate) && psnr(squaredError(reference, candidate), len(reference)) >= minPSNR {
			passing[i] = encoded
		}
		return nil
	})
	if err != nil {
		return err
	}
	_, err = dst.Write(smallest(original, passing))
	return err
}

func encodeJPEG(img image.Image, quality int) ([]byte, error) {
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, img, &jpeg.Options{Quality: quality}); err != nil {
		return nil, err
	}
	return encoded.Bytes(), nil
}

func compressSmart(dst io.Writer, original []byte, src image.Image, opts Options) error {
	minPSNR := opts.MinPSNR
	if minPSNR == 0 {
		minPSNR = 37
	}
	if minPSNR < 20 || minPSNR > 60 {
		return errors.New("minimum PSNR must be between 20 and 60 dB")
	}

	h := newHistogram(toNRGBA(src))
	// PSNR is measured against the decoded input. When its 8-bit form matches
	// the quantizer's input, which holds for everything except some paletted
	// images, the error can be summed per unique color instead of per pixel.
	reference := referencePixels(src)
	perColor := bytes.Equal(reference, h.img.Pix)
	if perColor {
		reference = nil
	}

	// Testing every candidate is intentional: median-cut quality is not
	// perfectly monotonic as bucket boundaries move.
	sizes := []int{256, 192, 160, 128, 96, 80, 64, 48, 32, 24, 16}
	passing := make([][]byte, len(sizes))
	err := forEach(len(sizes), func(i int) error {
		palette := h.palette(sizes[i])
		nearest := h.nearest(palette)
		var squared uint64
		if perColor {
			squared = h.colorError(palette, nearest)
		} else {
			squared = h.pixelError(reference, palette, nearest)
		}
		if psnr(squared, len(h.img.Pix)) < minPSNR {
			return nil
		}
		encoded, err := encodePNG(h.apply(palette, nearest))
		if err != nil {
			return err
		}
		passing[i] = encoded
		return nil
	})
	if err != nil {
		return err
	}
	_, err = dst.Write(smallest(original, passing))
	return err
}

// smallest returns the first strictly smaller candidate in order, or original.
func smallest(original []byte, candidates [][]byte) []byte {
	best := original
	for _, candidate := range candidates {
		if candidate != nil && len(candidate) < len(best) {
			best = candidate
		}
	}
	return best
}

// forEach calls f for 0 through n-1 on up to GOMAXPROCS goroutines and
// returns the error with the lowest index.
func forEach(n int, f func(int) error) error {
	errs := make([]error, n)
	var next atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < min(n, runtime.GOMAXPROCS(0)); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := int(next.Add(1) - 1); i < n; i = int(next.Add(1) - 1) {
				errs[i] = f(i)
			}
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func encodePNG(img image.Image) ([]byte, error) {
	var encoded bytes.Buffer
	encoder := png.Encoder{CompressionLevel: png.BestCompression}
	if err := encoder.Encode(&encoded, img); err != nil {
		return nil, err
	}
	return encoded.Bytes(), nil
}

func pixelPSNR(a, b image.Image) float64 {
	if !sameSize(a, b) {
		return 0
	}
	reference := referencePixels(a)
	return psnr(squaredError(reference, b), len(reference))
}

func sameSize(a, b image.Image) bool {
	return a.Bounds().Dx() == b.Bounds().Dx() && a.Bounds().Dy() == b.Bounds().Dy()
}

func psnr(squaredError uint64, samples int) float64 {
	if squaredError == 0 {
		return math.Inf(1)
	}
	mse := float64(squaredError) / float64(samples)
	return 10 * math.Log10(255*255/mse)
}

// referencePixels returns img as 8-bit NRGBA samples in row-major order,
// converted exactly as color.NRGBAModel converts each pixel.
func referencePixels(img image.Image) []uint8 {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if n, ok := img.(*image.NRGBA); ok && n.Stride == 4*w {
		start := n.PixOffset(b.Min.X, b.Min.Y)
		return n.Pix[start : start+4*w*h]
	}
	pix := make([]uint8, 4*w*h)
	for y := 0; y < h; y++ {
		nrgbaRow(img, y, pix[4*w*y:4*w*(y+1)])
	}
	return pix
}

// squaredError sums the squared sample differences between reference, as
// returned by referencePixels, and img.
func squaredError(reference []uint8, img image.Image) uint64 {
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	row := make([]uint8, 4*w)
	var sum uint64
	for y := 0; y < h; y++ {
		nrgbaRow(img, y, row)
		for i, v := range row {
			d := int(reference[4*w*y+i]) - int(v)
			sum += uint64(d * d)
		}
	}
	return sum
}

// nrgbaRow writes row y of img, counted from its bounds, to row. Fast paths
// cover the types the PNG and JPEG decoders return; each matches
// color.NRGBAModel exactly.
func nrgbaRow(img image.Image, y int, row []uint8) {
	b := img.Bounds()
	w := b.Dx()
	switch img := img.(type) {
	case *image.NRGBA:
		start := img.PixOffset(b.Min.X, b.Min.Y+y)
		copy(row, img.Pix[start:start+4*w])
	case *image.RGBA:
		for x := 0; x < w; x++ {
			r, g, bl, a := img.RGBAAt(b.Min.X+x, b.Min.Y+y).RGBA()
			putNRGBA(row[4*x:], r, g, bl, a)
		}
	case *image.YCbCr:
		for x := 0; x < w; x++ {
			r, g, bl, a := img.YCbCrAt(b.Min.X+x, b.Min.Y+y).RGBA()
			putNRGBA(row[4*x:], r, g, bl, a)
		}
	case *image.Gray:
		for x := 0; x < w; x++ {
			v := img.GrayAt(b.Min.X+x, b.Min.Y+y).Y
			row[4*x], row[4*x+1], row[4*x+2], row[4*x+3] = v, v, v, 0xff
		}
	default:
		for x := 0; x < w; x++ {
			c := color.NRGBAModel.Convert(img.At(b.Min.X+x, b.Min.Y+y)).(color.NRGBA)
			row[4*x], row[4*x+1], row[4*x+2], row[4*x+3] = c.R, c.G, c.B, c.A
		}
	}
}

// putNRGBA converts alpha-premultiplied 16-bit channels the way
// color.NRGBAModel does for colors that are not already color.NRGBA.
func putNRGBA(dst []uint8, r, g, b, a uint32) {
	switch a {
	case 0xffff:
		dst[0], dst[1], dst[2], dst[3] = uint8(r>>8), uint8(g>>8), uint8(b>>8), 0xff
	case 0:
		dst[0], dst[1], dst[2], dst[3] = 0, 0, 0, 0
	default:
		r = (r * 0xffff) / a
		g = (g * 0xffff) / a
		b = (b * 0xffff) / a
		dst[0], dst[1], dst[2], dst[3] = uint8(r>>8), uint8(g>>8), uint8(b>>8), uint8(a>>8)
	}
}

func toNRGBA(src image.Image) *image.NRGBA {
	bounds := src.Bounds()
	nrgba := image.NewNRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	draw.Draw(nrgba, nrgba.Bounds(), src, bounds.Min, draw.Src)
	return nrgba
}

func exactPalette(src image.Image, limit int) (*image.Paletted, bool) {
	// Palette entries are 8-bit; keep 16-bit PNGs in their original color model.
	switch src.(type) {
	case *image.Gray16, *image.RGBA64, *image.NRGBA64:
		return nil, false
	}
	nrgba := toNRGBA(src)

	indexes := make(map[uint32]uint8, limit)
	palette := make(color.Palette, 0, limit)
	for y := 0; y < nrgba.Rect.Dy(); y++ {
		for x := 0; x < nrgba.Rect.Dx(); x++ {
			i := nrgba.PixOffset(x, y)
			key := uint32(nrgba.Pix[i])<<24 | uint32(nrgba.Pix[i+1])<<16 |
				uint32(nrgba.Pix[i+2])<<8 | uint32(nrgba.Pix[i+3])
			if _, exists := indexes[key]; exists {
				continue
			}
			if len(palette) == limit {
				return nil, false
			}
			indexes[key] = uint8(len(palette))
			palette = append(palette, color.NRGBA{
				R: nrgba.Pix[i], G: nrgba.Pix[i+1], B: nrgba.Pix[i+2], A: nrgba.Pix[i+3],
			})
		}
	}

	out := image.NewPaletted(nrgba.Bounds(), palette)
	for y := 0; y < nrgba.Rect.Dy(); y++ {
		for x := 0; x < nrgba.Rect.Dx(); x++ {
			i := nrgba.PixOffset(x, y)
			key := uint32(nrgba.Pix[i])<<24 | uint32(nrgba.Pix[i+1])<<16 |
				uint32(nrgba.Pix[i+2])<<8 | uint32(nrgba.Pix[i+3])
			out.SetColorIndex(x, y, indexes[key])
		}
	}
	return out, true
}

// Quantize converts an image to an indexed palette using weighted median cut.
func Quantize(src image.Image, opts Options) (*image.Paletted, error) {
	if opts.Colors == 0 {
		opts.Colors = 256
	}
	if opts.Colors < 2 || opts.Colors > 256 {
		return nil, errors.New("colors must be between 2 and 256")
	}

	h := newHistogram(toNRGBA(src))
	palette := h.palette(opts.Colors)
	if opts.Dither {
		out := image.NewPaletted(h.img.Bounds(), toPalette(palette))
		dither(out, h.img, palette)
		return out, nil
	}
	return h.apply(palette, h.nearest(palette)), nil
}

func newHistogram(img *image.NRGBA) *histogram {
	index := make(map[uint32]uint32)
	var keys []uint32
	var counts []int
	pixels := make([]uint32, 0, img.Rect.Dx()*img.Rect.Dy())
	for y := 0; y < img.Rect.Dy(); y++ {
		for x := 0; x < img.Rect.Dx(); x++ {
			i := img.PixOffset(x, y)
			key := uint32(img.Pix[i])<<24 | uint32(img.Pix[i+1])<<16 |
				uint32(img.Pix[i+2])<<8 | uint32(img.Pix[i+3])
			id, ok := index[key]
			if !ok {
				id = uint32(len(keys))
				index[key] = id
				keys = append(keys, key)
				counts = append(counts, 0)
			}
			counts[id]++
			pixels = append(pixels, id)
		}
	}

	// Map iteration order is randomized in Go. Sorting by key establishes a
	// stable secondary order so equal channel values always split the same way.
	order := make([]uint32, len(keys))
	for i := range order {
		order[i] = uint32(i)
	}
	slices.SortFunc(order, func(a, b uint32) int { return cmpUint32(keys[a], keys[b]) })
	rank := make([]uint32, len(keys))
	samples := make([]sample, len(keys))
	for i, id := range order {
		rank[id] = uint32(i)
		key := keys[id]
		samples[i] = sample{r: uint8(key >> 24), g: uint8(key >> 16), b: uint8(key >> 8), a: uint8(key), count: counts[id]}
	}
	for i, id := range pixels {
		pixels[i] = rank[id]
	}
	return &histogram{img: img, samples: samples, pixels: pixels}
}

func cmpUint32(a, b uint32) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func (h *histogram) palette(limit int) []color.NRGBA {
	if len(h.samples) <= limit {
		return samplesToPalette(h.samples)
	}

	// Median cut reorders bucket contents in place; work on a copy so the
	// histogram can be shared between palette sizes.
	colors := slices.Clone(h.samples)
	buckets := []bucket{newBucket(colors, totalWeight(colors))}
	for len(buckets) < limit {
		idx := splittableBucket(buckets)
		if idx < 0 {
			break
		}
		left, right := splitBucket(buckets[idx])
		buckets[idx] = left
		buckets = append(buckets, right)
	}

	palette := make([]color.NRGBA, 0, len(buckets))
	for _, b := range buckets {
		palette = append(palette, averageColor(b.colors))
	}
	return refinePalette(h.samples, palette, 8)
}

// nearest returns the palette index for every unique color.
func (h *histogram) nearest(palette []color.NRGBA) []uint8 {
	search := newPaletteSearch(palette)
	indexes := make([]uint8, len(h.samples))
	for i, s := range h.samples {
		indexes[i] = search.nearest(s.r, s.g, s.b, s.a)
	}
	return indexes
}

func (h *histogram) apply(palette []color.NRGBA, nearest []uint8) *image.Paletted {
	out := image.NewPaletted(h.img.Bounds(), toPalette(palette))
	for i, id := range h.pixels {
		out.Pix[i] = nearest[id]
	}
	return out
}

// colorError is the squared error of mapping the image to palette, summed
// once per unique color.
func (h *histogram) colorError(palette []color.NRGBA, nearest []uint8) uint64 {
	var sum uint64
	for i, s := range h.samples {
		c := palette[nearest[i]]
		dr, dg, db, da := int(s.r)-int(c.R), int(s.g)-int(c.G), int(s.b)-int(c.B), int(s.a)-int(c.A)
		sum += uint64(dr*dr+dg*dg+db*db+da*da) * uint64(s.count)
	}
	return sum
}

// pixelError is the squared error of mapping the image to palette, measured
// against reference pixels from referencePixels.
func (h *histogram) pixelError(reference []uint8, palette []color.NRGBA, nearest []uint8) uint64 {
	var sum uint64
	for i, id := range h.pixels {
		c := palette[nearest[id]]
		p := reference[4*i : 4*i+4]
		dr, dg, db, da := int(p[0])-int(c.R), int(p[1])-int(c.G), int(p[2])-int(c.B), int(p[3])-int(c.A)
		sum += uint64(dr*dr + dg*dg + db*db + da*da)
	}
	return sum
}

func refinePalette(samples []sample, palette []color.NRGBA, iterations int) []color.NRGBA {
	for iteration := 0; iteration < iterations; iteration++ {
		search := newPaletteSearch(palette)
		type sum struct{ r, g, b, a, weight uint64 }
		sums := make([]sum, len(palette))
		for _, s := range samples {
			idx := search.nearest(s.r, s.g, s.b, s.a)
			w := uint64(s.count)
			sums[idx].r += uint64(s.r) * w
			sums[idx].g += uint64(s.g) * w
			sums[idx].b += uint64(s.b) * w
			sums[idx].a += uint64(s.a) * w
			sums[idx].weight += w
		}
		changed := false
		for i, s := range sums {
			if s.weight == 0 {
				continue
			}
			updated := color.NRGBA{
				R: uint8(s.r / s.weight), G: uint8(s.g / s.weight),
				B: uint8(s.b / s.weight), A: uint8(s.a / s.weight),
			}
			if palette[i] != updated {
				palette[i], changed = updated, true
			}
		}
		if !changed {
			break
		}
	}
	return palette
}

func samplesToPalette(colors []sample) []color.NRGBA {
	p := make([]color.NRGBA, 0, len(colors))
	for _, c := range colors {
		p = append(p, color.NRGBA{R: c.r, G: c.g, B: c.b, A: c.a})
	}
	return p
}

func toPalette(colors []color.NRGBA) color.Palette {
	p := make(color.Palette, len(colors))
	for i, c := range colors {
		p[i] = c
	}
	return p
}

func totalWeight(colors []sample) int {
	total := 0
	for _, c := range colors {
		total += c.count
	}
	return total
}

func channelValue(c sample, channel int) uint8 {
	switch channel {
	case 0:
		return c.r
	case 1:
		return c.g
	case 2:
		return c.b
	default:
		return c.a
	}
}

func newBucket(colors []sample, weight int) bucket {
	var lo, hi [4]uint8
	lo = [4]uint8{255, 255, 255, 255}
	for _, c := range colors {
		for ch, v := range [4]uint8{c.r, c.g, c.b, c.a} {
			lo[ch] = min(lo[ch], v)
			hi[ch] = max(hi[ch], v)
		}
	}
	b := bucket{colors: colors, weight: weight, spread: -1}
	for ch := 0; ch < 4; ch++ {
		if r := int(hi[ch]) - int(lo[ch]); r > b.spread {
			b.channel, b.spread = ch, r
		}
	}
	return b
}

func splittableBucket(buckets []bucket) int {
	best, score := -1, -1
	for i, b := range buckets {
		if len(b.colors) < 2 {
			continue
		}
		candidate := b.spread * b.weight
		if candidate > score {
			best, score = i, candidate
		}
	}
	return best
}

func splitBucket(b bucket) (bucket, bucket) {
	slices.SortStableFunc(b.colors, func(x, y sample) int {
		return int(channelValue(x, b.channel)) - int(channelValue(y, b.channel))
	})

	half, cumulative, cut := b.weight/2, 0, 1
	for i := 0; i < len(b.colors)-1; i++ {
		cumulative += b.colors[i].count
		if cumulative >= half {
			cut = i + 1
			break
		}
	}
	left, right := b.colors[:cut:cut], b.colors[cut:]
	leftWeight := totalWeight(left)
	return newBucket(left, leftWeight), newBucket(right, b.weight-leftWeight)
}

func averageColor(colors []sample) color.NRGBA {
	var r, g, b, a, total uint64
	for _, c := range colors {
		w := uint64(c.count)
		r += uint64(c.r) * w
		g += uint64(c.g) * w
		b += uint64(c.b) * w
		a += uint64(c.a) * w
		total += w
	}
	return color.NRGBA{uint8(r / total), uint8(g / total), uint8(b / total), uint8(a / total)}
}

func projection(r, g, b, a uint8) int {
	return int(r) + int(g) + int(b) + 2*int(a)
}

func newPaletteSearch(palette []color.NRGBA) *paletteSearch {
	order := make([]uint8, len(palette))
	for i := range order {
		order[i] = uint8(i)
	}
	key := func(i uint8) int { c := palette[i]; return projection(c.R, c.G, c.B, c.A) }
	sort.Slice(order, func(i, j int) bool { return key(order[i]) < key(order[j]) })
	keys := make([]int, len(order))
	for i, idx := range order {
		keys[i] = key(idx)
	}
	return &paletteSearch{colors: palette, order: order, keys: keys}
}

// nearest returns the index of the closest palette entry, preferring the
// lowest index on ties, exactly as a linear scan would.
func (p *paletteSearch) nearest(r, g, b, a uint8) uint8 {
	key := projection(r, g, b, a)
	start := sort.SearchInts(p.keys, key)
	best, bestIndex := 1<<40, 0
	check := func(i int) bool {
		gap := p.keys[i] - key
		if gap*gap > 5*best {
			return false
		}
		idx := int(p.order[i])
		c := p.colors[idx]
		dr, dg, db, da := int(r)-int(c.R), int(g)-int(c.G), int(b)-int(c.B), int(a)-int(c.A)
		// Alpha gets extra weight because mismatching transparency is conspicuous.
		distance := dr*dr + dg*dg + db*db + 2*da*da
		if distance < best || (distance == best && idx < bestIndex) {
			best, bestIndex = distance, idx
		}
		return true
	}
	for i := start; i < len(p.keys) && check(i); i++ {
	}
	for i := start - 1; i >= 0 && check(i); i-- {
	}
	return uint8(bestIndex)
}

func dither(dst *image.Paletted, src *image.NRGBA, palette []color.NRGBA) {
	search := newPaletteSearch(palette)
	w, h := src.Rect.Dx(), src.Rect.Dy()
	current := make([][4]float64, w+2)
	next := make([][4]float64, w+2)
	clamp := func(v float64) uint8 {
		if v < 0 {
			return 0
		}
		if v > 255 {
			return 255
		}
		return uint8(v + 0.5)
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i := src.PixOffset(x, y)
			r := clamp(float64(src.Pix[i]) + current[x+1][0])
			g := clamp(float64(src.Pix[i+1]) + current[x+1][1])
			b := clamp(float64(src.Pix[i+2]) + current[x+1][2])
			a := src.Pix[i+3] // Do not diffuse alpha; it creates edge artifacts.
			pi := search.nearest(r, g, b, a)
			dst.SetColorIndex(x, y, pi)
			pc := palette[pi]
			err := [4]float64{float64(r) - float64(pc.R), float64(g) - float64(pc.G), float64(b) - float64(pc.B), 0}
			for ch := 0; ch < 3; ch++ {
				current[x+2][ch] += err[ch] * 7 / 16
				next[x][ch] += err[ch] * 3 / 16
				next[x+1][ch] += err[ch] * 5 / 16
				next[x+2][ch] += err[ch] / 16
			}
		}
		current, next = next, current
		for i := range next {
			next[i] = [4]float64{}
		}
	}
}
