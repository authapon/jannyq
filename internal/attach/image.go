package attach

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"

	_ "image/gif" // register decoders
	_ "image/png"

	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

// jpegQuality is high enough for text in screenshots to stay legible.
const jpegQuality = 88

// imageResult is a picture prepared for the model.
type imageResult struct {
	JPEG          []byte
	Width, Height int // as sent
	OrigW, OrigH  int // as received
}

// prepareImage decodes a picture, checks its size before allocating memory
// for it, applies the EXIF orientation, shrinks it so that its longer edge is at
// most maxEdge, flattens transparency onto white and re-encodes it as JPEG.
// Re-encoding also drops all metadata (GPS position, camera, ...), which the
// model has no need of and the sender may not want shared.
func prepareImage(data []byte, maxEdge int, maxPixels int64) (res imageResult, err error) {
	defer func() {
		if r := recover(); r != nil {
			res, err = imageResult{}, ErrCorrupt
		}
	}()
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return res, ErrCorrupt
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > maxPixels {
		return res, ErrImageTooBig
	}
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return res, ErrCorrupt
	}
	res.OrigW, res.OrigH = src.Bounds().Dx(), src.Bounds().Dy()

	if edge := max(res.OrigW, res.OrigH); maxEdge > 0 && edge > maxEdge {
		w, h := res.OrigW*maxEdge/edge, res.OrigH*maxEdge/edge
		dst := image.NewRGBA(image.Rect(0, 0, max(w, 1), max(h, 1)))
		xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), xdraw.Over, nil)
		src = dst
	}
	src = orient(src, exifOrientation(data))

	// flatten onto white: JPEG has no alpha, and black would hide dark drawings
	flat := image.NewRGBA(src.Bounds())
	draw.Draw(flat, flat.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	draw.Draw(flat, flat.Bounds(), src, src.Bounds().Min, draw.Over)

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, flat, &jpeg.Options{Quality: jpegQuality}); err != nil {
		return res, fmt.Errorf("attach: encoding the image: %w", err)
	}
	res.JPEG = buf.Bytes()
	res.Width, res.Height = flat.Bounds().Dx(), flat.Bounds().Dy()
	return res, nil
}

// exifOrientation reads the orientation (1–8) from a JPEG's EXIF data, or 1.
func exifOrientation(data []byte) int {
	if len(data) < 4 || data[0] != 0xff || data[1] != 0xd8 {
		return 1
	}
	i := 2
	for i+4 <= len(data) {
		if data[i] != 0xff {
			return 1
		}
		marker := data[i+1]
		if marker == 0xda || marker == 0xd9 { // start of scan / end of image: no EXIF beyond here
			return 1
		}
		size := int(binary.BigEndian.Uint16(data[i+2 : i+4]))
		if size < 2 || i+2+size > len(data) {
			return 1
		}
		seg := data[i+4 : i+2+size]
		if marker == 0xe1 && len(seg) > 14 && bytes.HasPrefix(seg, []byte("Exif\x00\x00")) {
			return tiffOrientation(seg[6:])
		}
		i += 2 + size
	}
	return 1
}

func tiffOrientation(t []byte) int {
	if len(t) < 8 {
		return 1
	}
	var bo binary.ByteOrder
	switch string(t[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return 1
	}
	off := int(bo.Uint32(t[4:8]))
	if off < 8 || off+2 > len(t) {
		return 1
	}
	n := int(bo.Uint16(t[off : off+2]))
	for k := 0; k < n; k++ {
		e := off + 2 + k*12
		if e+12 > len(t) {
			return 1
		}
		if bo.Uint16(t[e:e+2]) == 0x0112 { // Orientation
			if v := int(bo.Uint16(t[e+8 : e+10])); v >= 1 && v <= 8 {
				return v
			}
			return 1
		}
	}
	return 1
}

// orient turns a picture the right way up according to an EXIF orientation.
func orient(img image.Image, o int) image.Image {
	if o <= 1 || o > 8 {
		return img
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	swap := o >= 5
	dw, dh := w, h
	if swap {
		dw, dh = h, w
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var nx, ny int
			switch o {
			case 2: // mirror horizontally
				nx, ny = w-1-x, y
			case 3: // rotate 180
				nx, ny = w-1-x, h-1-y
			case 4: // mirror vertically
				nx, ny = x, h-1-y
			case 5: // transpose
				nx, ny = y, x
			case 6: // rotate 90 clockwise
				nx, ny = h-1-y, x
			case 7: // transverse
				nx, ny = h-1-y, w-1-x
			case 8: // rotate 90 counter-clockwise
				nx, ny = y, w-1-x
			}
			dst.Set(nx, ny, img.At(b.Min.X+x, b.Min.Y+y))
		}
	}
	return dst
}
