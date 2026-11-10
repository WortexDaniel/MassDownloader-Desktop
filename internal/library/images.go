package library

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"math"
	"os"
	"sort"
	"strings"

	"github.com/HugoSmits86/nativewebp"
	_ "golang.org/x/image/bmp"
	"golang.org/x/image/draw"
	_ "golang.org/x/image/tiff"
)

const maxPixels int64 = 64_000_000
const maxImageBytes int64 = 512 << 20

func imageExtension(ext string) bool {
	switch ext {
	case "jpg", "jpeg", "png", "gif", "webp", "avif", "bmp", "tif", "tiff":
		return true
	}
	return false
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}
func hashFile(ctx context.Context, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, contextReader{ctx, f}); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func inspect(ctx context.Context, f *File) (resultErr error) {
	st, err := os.Stat(f.Path)
	if err != nil {
		return err
	}
	defer func() {
		if resultErr != nil {
			return
		}
		now, e := os.Stat(f.Path)
		if e != nil {
			resultErr = e
		} else if now.Size() != st.Size() || now.ModTime() != st.ModTime() {
			resultErr = fmt.Errorf("файл изменился во время анализа: %s", f.Path)
		}
	}()
	f.Size = st.Size()
	f.Modified = st.ModTime().UnixNano()
	f.Error = ""
	f.Pixels = ""
	f.Hash = ""
	f.Color = nil
	f.Width = 0
	f.Height = 0
	f.BitDepth = 0
	f.Frames = 1
	f.Profile = false
	f.Orientation = 1
	f.SHA, err = hashFile(ctx, f.Path)
	if err != nil {
		return err
	}
	input, err := os.Open(f.Path)
	if err != nil {
		return err
	}
	defer input.Close()
	conf, format, err := image.DecodeConfig(input)
	if err != nil {
		f.Format = strings.ToLower(strings.TrimPrefix(filepathExt(f.Path), "."))
		f.Error = "нет декодера для анализа пикселей: " + err.Error()
		return nil
	}
	f.Format = format
	if format == "jpeg" {
		f.Format = "jpg"
	}
	f.Width = conf.Width
	f.Height = conf.Height
	f.BitDepth = 8
	if conf.ColorModel == color.NRGBA64Model || conf.ColorModel == color.RGBA64Model || conf.ColorModel == color.Gray16Model {
		f.BitDepth = 16
	}
	meta, err := readMetadata(f.Path)
	if err != nil {
		f.Error = err.Error()
		return nil
	}
	f.Profile = len(meta.ICC) > 0
	f.Orientation = exifValue(meta.EXIF, 274)
	if format == "gif" {
		f.Frames, err = gifFrames(f.Path)
	} else {
		f.Frames = meta.Frames
	}
	if err != nil {
		f.Error = err.Error()
		return nil
	}
	if f.Frames > 1 {
		f.Error = "анимация или несколько страниц: доступны имя и точное содержимое; сравнение кадров не выполняется"
		return nil
	}
	if f.Size > maxImageBytes || conf.Width <= 0 || conf.Height <= 0 || int64(conf.Width)*int64(conf.Height) > maxPixels {
		f.Error = "превышен лимит анализа: 64 Мп или 512 МБ"
		return nil
	}
	if _, err = input.Seek(0, io.SeekStart); err != nil {
		return err
	}
	img, _, err := image.Decode(contextReader{ctx, input})
	if err != nil {
		f.Error = err.Error()
		return nil
	}
	img = orient(img, f.Orientation)
	f.Width = img.Bounds().Dx()
	f.Height = img.Bounds().Dy()
	h := sha256.New()
	var sizes [16]byte
	binary.LittleEndian.PutUint64(sizes[:8], uint64(f.Width))
	binary.LittleEndian.PutUint64(sizes[8:], uint64(f.Height))
	h.Write(sizes[:])
	h.Write(meta.ICC)
	row := make([]byte, f.Width*8)
	for y := 0; y < f.Height; y++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		for x := 0; x < f.Width; x++ {
			pixel := img.At(img.Bounds().Min.X+x, img.Bounds().Min.Y+y)
			c := color.NRGBA64Model.Convert(pixel).(color.NRGBA64)
			if f.BitDepth != 16 {
				n := color.NRGBAModel.Convert(pixel).(color.NRGBA)
				c = color.NRGBA64{R: uint16(n.R) * 257, G: uint16(n.G) * 257, B: uint16(n.B) * 257, A: uint16(n.A) * 257}
			}
			i := x * 8
			binary.LittleEndian.PutUint16(row[i:], c.R)
			binary.LittleEndian.PutUint16(row[i+2:], c.G)
			binary.LittleEndian.PutUint16(row[i+4:], c.B)
			binary.LittleEndian.PutUint16(row[i+6:], c.A)
		}
		h.Write(row)
	}
	f.Pixels = hex.EncodeToString(h.Sum(nil))
	f.Hash, f.Color = perceptual(img)
	return nil
}
func filepathExt(path string) string {
	if i := strings.LastIndexAny(path, "/\\"); i >= 0 {
		path = path[i+1:]
	}
	if i := strings.LastIndexByte(path, '.'); i >= 0 {
		return path[i:]
	}
	return ""
}
func orient(src image.Image, o int) image.Image {
	if o < 2 || o > 8 {
		return src
	}
	w, h := src.Bounds().Dx(), src.Bounds().Dy()
	dw, dh := w, h
	if o >= 5 {
		dw, dh = h, w
	}
	var dst draw.Image = image.NewNRGBA(image.Rect(0, 0, dw, dh))
	if src.ColorModel() == color.NRGBA64Model || src.ColorModel() == color.RGBA64Model || src.ColorModel() == color.Gray16Model {
		dst = image.NewNRGBA64(image.Rect(0, 0, dw, dh))
	}
	for y := 0; y < dh; y++ {
		for x := 0; x < dw; x++ {
			sx, sy := x, y
			switch o {
			case 2:
				sx = w - 1 - x
			case 3:
				sx, sy = w-1-x, h-1-y
			case 4:
				sy = h - 1 - y
			case 5:
				sx, sy = y, x
			case 6:
				sx, sy = y, h-1-x
			case 7:
				sx, sy = w-1-y, h-1-x
			case 8:
				sx, sy = w-1-y, x
			}
			dst.Set(x, y, src.At(src.Bounds().Min.X+sx, src.Bounds().Min.Y+sy))
		}
	}
	return dst
}
func perceptual(img image.Image) (string, []int) {
	small := image.NewNRGBA(image.Rect(0, 0, 32, 32))
	draw.ApproxBiLinear.Scale(small, small.Bounds(), img, img.Bounds(), draw.Src, nil)
	var values [32][32]float64
	colorSum := []int{0, 0, 0, 0}
	for y := 0; y < 32; y++ {
		for x := 0; x < 32; x++ {
			c := small.NRGBAAt(x, y)
			a := float64(c.A) / 255
			r := float64(c.R)*a + 255*(1-a)
			g := float64(c.G)*a + 255*(1-a)
			b := float64(c.B)*a + 255*(1-a)
			values[y][x] = .299*r + .587*g + .114*b
			colorSum[0] += int(r)
			colorSum[1] += int(g)
			colorSum[2] += int(b)
			colorSum[3] += int(c.A)
		}
	}
	for i := range colorSum {
		colorSum[i] /= 1024
	}
	var cosines [8][32]float64
	for u := 0; u < 8; u++ {
		for x := 0; x < 32; x++ {
			cosines[u][x] = math.Cos(float64(2*x+1) * float64(u) * math.Pi / 64)
		}
	}
	coeff := make([]float64, 0, 63)
	for v := 0; v < 8; v++ {
		for u := 0; u < 8; u++ {
			if u == 0 && v == 0 {
				continue
			}
			sum := 0.
			for y := 0; y < 32; y++ {
				for x := 0; x < 32; x++ {
					sum += values[y][x] * cosines[u][x] * cosines[v][y]
				}
			}
			coeff = append(coeff, sum)
		}
	}
	sorted := append([]float64{}, coeff...)
	sort.Float64s(sorted)
	median := sorted[len(sorted)/2]
	var hash uint64
	for i, c := range coeff {
		if c > median {
			hash |= uint64(1) << i
		}
	}
	return fmt.Sprintf("%016x", hash), colorSum
}
func gifFrames(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	r := bufio.NewReader(f)
	header := make([]byte, 13)
	if _, err = io.ReadFull(r, header); err != nil {
		return 0, err
	}
	skip := func(n int) error { _, e := io.CopyN(io.Discard, r, int64(n)); return e }
	if header[10]&128 != 0 {
		if err = skip(3 * (1 << ((header[10] & 7) + 1))); err != nil {
			return 0, err
		}
	}
	subblocks := func() error {
		for {
			n, e := r.ReadByte()
			if e != nil {
				return e
			}
			if n == 0 {
				return nil
			}
			if e = skip(int(n)); e != nil {
				return e
			}
		}
	}
	frames := 0
	for {
		b, e := r.ReadByte()
		if e != nil {
			return frames, e
		}
		switch b {
		case 0x3b:
			return max(1, frames), nil
		case 0x21:
			if _, e = r.ReadByte(); e == nil {
				e = subblocks()
			}
		case 0x2c:
			desc := make([]byte, 9)
			_, e = io.ReadFull(r, desc)
			if e == nil && desc[8]&128 != 0 {
				e = skip(3 * (1 << ((desc[8] & 7) + 1)))
			}
			if e == nil {
				_, e = r.ReadByte()
			}
			if e == nil {
				e = subblocks()
			}
			frames++
		default:
			e = fmt.Errorf("повреждённый GIF")
		}
		if e != nil {
			return frames, e
		}
		if frames > 100000 {
			return frames, fmt.Errorf("слишком много кадров")
		}
	}
}

type Conversion struct {
	Format         string `json:"format"`
	Quality        int    `json:"quality"`
	MaxWidth       int    `json:"max_width"`
	MaxHeight      int    `json:"max_height"`
	Replace        bool   `json:"replace"`
	Metadata       bool   `json:"metadata"`
	Background     string `json:"background"`
	AllowDepthLoss bool   `json:"allow_depth_loss"`
	thumbnail      bool
}

func convert(ctx context.Context, source, target string, o Conversion) error {
	f, err := os.Open(source)
	if err != nil {
		return err
	}
	defer f.Close()
	conf, sourceFormat, err := image.DecodeConfig(f)
	if err != nil {
		return err
	}
	if int64(conf.Width)*int64(conf.Height) > maxPixels {
		return fmt.Errorf("изображение превышает лимит 64 Мп")
	}
	if _, err = f.Seek(0, 0); err != nil {
		return err
	}
	img, _, err := image.Decode(contextReader{ctx, f})
	if err != nil {
		return err
	}
	meta, err := readMetadata(source)
	if err != nil {
		return err
	}
	if conf.ColorModel == color.CMYKModel && len(meta.ICC) > 0 {
		return fmt.Errorf("конвертация CMYK с ICC-профилем требует отдельного преобразования цвета")
	}
	if sourceFormat == "tiff" && o.Metadata && !o.thumbnail {
		return fmt.Errorf("сохранение полного EXIF из TIFF не поддерживается; отключите сохранение метаданных для конвертации")
	}
	if meta.Frames > 1 {
		return fmt.Errorf("многостраничное изображение не преобразуется в один кадр")
	}
	if sourceFormat == "gif" {
		frames, e := gifFrames(source)
		if e != nil {
			return e
		}
		if frames > 1 {
			return fmt.Errorf("анимация не преобразуется в один кадр")
		}
	}
	if o.Format == "webp" && !o.AllowDepthLoss && (conf.ColorModel == color.NRGBA64Model || conf.ColorModel == color.RGBA64Model || conf.ColorModel == color.Gray16Model) {
		return fmt.Errorf("WebP поддерживает 8 бит на канал; подтвердите перевод из 16 бит в настройках")
	}
	img = orient(img, exifValue(meta.EXIF, 274))
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	scale := 1.
	if o.MaxWidth > 0 {
		scale = math.Min(scale, float64(o.MaxWidth)/float64(w))
	}
	if o.MaxHeight > 0 {
		scale = math.Min(scale, float64(o.MaxHeight)/float64(h))
	}
	if scale < 1 {
		nw, nh := max(1, int(float64(w)*scale)), max(1, int(float64(h)*scale))
		var dst draw.Image = image.NewNRGBA(image.Rect(0, 0, nw, nh))
		if conf.ColorModel == color.NRGBA64Model || conf.ColorModel == color.RGBA64Model || conf.ColorModel == color.Gray16Model {
			dst = image.NewNRGBA64(image.Rect(0, 0, nw, nh))
		}
		draw.CatmullRom.Scale(dst, dst.Bounds(), img, img.Bounds(), draw.Src, nil)
		img = dst
		w, h = nw, nh
	}
	if o.Format == "jpg" {
		bg := color.NRGBA{255, 255, 255, 255}
		if o.Background == "black" {
			bg = color.NRGBA{0, 0, 0, 255}
		}
		dst := image.NewRGBA(image.Rect(0, 0, w, h))
		draw.Draw(dst, dst.Bounds(), &image.Uniform{C: bg}, image.Point{}, draw.Src)
		draw.Draw(dst, dst.Bounds(), img, img.Bounds().Min, draw.Over)
		img = dst
	}
	out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		out.Close()
		if !ok {
			os.Remove(target)
		}
	}()
	writer := contextWriter{ctx, out}
	switch o.Format {
	case "jpg":
		err = jpeg.Encode(writer, img, &jpeg.Options{Quality: o.Quality})
	case "png":
		encoder := png.Encoder{CompressionLevel: png.DefaultCompression}
		err = encoder.Encode(writer, img)
	case "webp":
		err = nativewebp.Encode(writer, img, nil)
	default:
		err = fmt.Errorf("неподдерживаемый выходной формат")
	}
	if err != nil {
		return err
	}
	if err = out.Sync(); err != nil {
		return err
	}
	if err = out.Close(); err != nil {
		return err
	}
	if o.Metadata {
		patchExif(meta.EXIF, w, h)
		if err = writeMetadata(target, o.Format, meta, w, h); err != nil {
			return err
		}
	}
	ok = true
	return nil
}

type contextWriter struct {
	ctx context.Context
	w   io.Writer
}

func WriteThumbnail(ctx context.Context, source, target string) error {
	return convert(ctx, source, target, Conversion{Format: "jpg", Quality: 82, MaxWidth: 420, MaxHeight: 320, Metadata: true, thumbnail: true})
}

func (w contextWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	return w.w.Write(p)
}

var _ = gif.GIF{} // register GIF decoding, including single-frame GIFs.
