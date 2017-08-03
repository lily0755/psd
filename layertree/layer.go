// Package layertree implements PSD image drawing function.
//
// This package supports only RGBA color mode.
package layertree

import (
	"context"
	"image"
	"io"
	"runtime"

	"github.com/oov/psd"

	"golang.org/x/text/encoding"
)

// Root represents root of the layer tree.
type Root struct {
	Renderer *Renderer

	tileSize int

	layerImage map[int]layerImage

	Rect       image.Rectangle
	CanvasRect image.Rectangle

	Children []Layer
}

// Clone creates copy of r.
func (r *Root) Clone() *Root {
	return cloner{}.Clone(r)
}

// Layer represents the layer image.
type Layer struct {
	SeqID int

	Name        string
	DisplayName string

	Folder     bool
	FolderOpen bool

	Visible   bool
	BlendMode psd.BlendMode
	Opacity   int // 0-255
	Clipping  bool

	BlendClippedElements bool

	Rect image.Rectangle

	MaskEnabled      bool
	MaskRect         image.Rectangle
	MaskDefaultColor int // 0 or 255

	Parent   *Layer
	Children []Layer

	ClippedBy *Layer
	Clip      []*Layer
}

const DefaultTileSize = 64

type Options struct {
	TileSize int
	// It will used to detect character encoding of a variable-width encoding layer name.
	LayerNameEncodingDetector func([]byte) encoding.Encoding
}

// New creates layer tree from the psdFile.
//
// New can cancel processing through ctx.
// If you pass 0 to opt.TileSize, it will be DefaultTileSize.
func New(ctx context.Context, psdFile io.Reader, opt *Options) (*Root, error) {
	if opt == nil {
		opt = &Options{}
	}
	if opt.TileSize == 0 {
		opt.TileSize = DefaultTileSize
	}
	if opt.LayerNameEncodingDetector == nil {
		opt.LayerNameEncodingDetector = func([]byte) encoding.Encoding { return encoding.Nop }
	}

	layerImages, img, err := createCanvas(ctx, psdFile, opt.TileSize)
	if err != nil {
		return nil, err
	}

	b := &builder{
		Img: img,
		LayerNameEncodingDetector: opt.LayerNameEncodingDetector,
	}
	var layers []Layer
	for i := range img.Layer {
		layers = append(layers, Layer{})
		b.buildLayer(&layers[i], &img.Layer[i])
	}
	b.registerClippingGroup(layers)

	r := &Root{
		Renderer: &Renderer{
			rootImage: tiledImage{},
			cached:    map[image.Point]struct{}{},
		},
		tileSize:   opt.TileSize,
		layerImage: layerImages,

		CanvasRect: img.Config.Rect,
		Rect:       b.Rect.Intersect(img.Config.Rect),

		Children: layers,
	}
	r.Renderer.pool.New = r.Renderer.allocate
	r.Renderer.layertree = r

	return r, nil
}

func createCanvas(ctx context.Context, psdFile io.Reader, tileSize int) (map[int]layerImage, *psd.PSD, error) {
	/*
		if img.Config.ColorMode != psd.ColorModeRGB {
			return errors.New("Unsupported color mode")
		}
	*/

	n := runtime.GOMAXPROCS(0)

	ch := make(chan *psd.Layer)
	layerImages := map[int]layerImage{}

	cctx, cancel := context.WithCancel(ctx)
	defer cancel()

	pc := &parallelContext{}
	pc.Wg.Add(n)
	for i := 0; i < n; i++ {
		go createCanvasInner(cctx, pc, ch, tileSize, layerImages)
	}
	img, _, err := psd.Decode(psdFile, &psd.DecodeOptions{
		SkipMergedImage:  true,
		LayerImageLoaded: func(l *psd.Layer, index int, total int) { ch <- l },
	})
	close(ch)
	if err != nil {
		return nil, nil, err
	}
	if err = pc.Wait(ctx); err != nil {
		return nil, nil, err
	}
	return layerImages, img, nil
}

func createCanvasInner(ctx context.Context, pc *parallelContext, ch <-chan *psd.Layer, tileSize int, layerImages map[int]layerImage) {
	defer pc.Done()
	for l := range ch {
		var ld layerImage
		if l.HasImage() && !l.Rect.Empty() {
			r, g, b := l.Channel[0].Data, l.Channel[1].Data, l.Channel[2].Data
			var a []byte
			if ach, ok := l.Channel[-1]; ok {
				a = ach.Data
			}
			if err := ld.Canvas.Store(ctx, tileSize, l.Rect, r, g, b, a); err != nil {
				return
			}
		}
		if !l.Mask.Rect.Empty() {
			if a, ok := l.Channel[-2]; ok {
				if err := ld.Mask.Store(ctx, tileSize, l.Mask.Rect, a.Data, l.Mask.DefaultColor); err != nil {
					return
				}
			}
		}

		pc.M.Lock()
		layerImages[l.SeqID] = ld
		pc.M.Unlock()
	}
}