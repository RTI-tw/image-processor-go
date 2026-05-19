package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	imagedraw "image/draw"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"cloud.google.com/go/storage"
	webp "github.com/gen2brain/webp"
	exifpkg "github.com/rwcarlsen/goexif/exif"
	xdraw "golang.org/x/image/draw"
	xtiff "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"
)

const sourceGenerationMetadataKey = "sourceGeneration"

var trailingResizeLabelPattern = regexp.MustCompile(`(?:^|.*-)(w\d{2,})$`)

type Processor struct {
	cfg       Config
	storage   *storage.Client
	watermark *image.NRGBA
}

func NewProcessor(cfg Config, storageClient *storage.Client) (*Processor, error) {
	p := &Processor{
		cfg:     cfg,
		storage: storageClient,
	}
	if !cfg.EnableWatermark {
		return p, nil
	}

	wmBytes, err := os.ReadFile(cfg.WatermarkPath)
	if err != nil {
		return nil, fmt.Errorf("read watermark: %w", err)
	}
	img, _, err := image.Decode(bytes.NewReader(wmBytes))
	if err != nil {
		return nil, fmt.Errorf("decode watermark: %w", err)
	}
	p.watermark = toNRGBA(img)
	return p, nil
}

func (p *Processor) Process(ctx context.Context, event storageEvent) error {
	p.logMemory(event.Name, "start")
	if !isSupportedImage(event.Name) {
		log.Printf("skip unsupported object: %s", event.Name)
		return nil
	}
	if isDerivedObjectName(event.Name) {
		log.Printf("skip derived object: %s", event.Name)
		return nil
	}

	base := filepath.Base(event.Name)
	ext := filepath.Ext(base)
	imageFileID := strings.TrimSuffix(base, ext)
	baseDir := strings.TrimSuffix(event.Name, base)

	originalWebPName := baseDir + imageFileID + ".webP"
	completionSentinelName := completionSentinelObjectName(baseDir, imageFileID, originalWebPName, p.cfg.ResizeTargets)
	alreadyProcessed, err := p.alreadyProcessedSourceGeneration(ctx, event.Bucket, completionSentinelName, event.Generation)
	if err != nil {
		return err
	}
	if alreadyProcessed {
		log.Printf("skip already processed source generation: object=%s generation=%s", event.Name, event.Generation)
		return nil
	}

	sourceObject := p.sourceObject(event.Bucket, event.Name, event.Generation)
	reader, err := sourceObject.NewReader(ctx)
	if err != nil {
		return fmt.Errorf("open object: %w", err)
	}
	defer reader.Close()

	originalBytes, err := io.ReadAll(reader)
	if err != nil {
		return fmt.Errorf("read object: %w", err)
	}
	p.logMemory(event.Name, fmt.Sprintf("read original bytes=%d", len(originalBytes)))
	if p.cfg.MaxSourcePixels > 0 {
		if imgCfg, _, err := image.DecodeConfig(bytes.NewReader(originalBytes)); err == nil {
			if err := validateSourceImageSize(imgCfg.Width, imgCfg.Height, p.cfg.MaxSourcePixels); err != nil {
				return err
			}
		}
	}
	sourceImg, _, err := image.Decode(bytes.NewReader(originalBytes))
	if err != nil {
		return fmt.Errorf("decode image: %w", err)
	}
	sourceImg = applyEXIFOrientation(sourceImg, originalBytes)
	originalBytes = nil
	p.logMemory(event.Name, fmt.Sprintf("decoded source bounds=%s", sourceImg.Bounds()))

	for _, target := range p.cfg.ResizeTargets {
		shouldCopyMain := !p.cfg.EnableWatermark && sourceImg.Bounds().Dx() <= target.Width
		var targetImg image.Image = sourceImg
		var resized *image.NRGBA
		if !shouldCopyMain {
			resized = resizeImage(sourceImg, target.Width)
			targetImg = resized
		}
		if p.cfg.EnableWatermark && resized != nil {
			resized = applyWatermark(resized, p.watermark, p.cfg.WatermarkScale, p.cfg.WatermarkMarginRatio, p.cfg.WatermarkOpacity)
			targetImg = resized
		}
		p.logMemory(event.Name, fmt.Sprintf("prepared target=%s bounds=%s", target.Label, targetImg.Bounds()))

		mainObjectName := baseDir + imageFileID + "-" + target.Label + ext
		if shouldCopyMain {
			if err := p.copyObject(ctx, event.Bucket, sourceObject, mainObjectName, contentTypeFromExt(ext), event.Generation); err != nil {
				return err
			}
			p.logMemory(event.Name, fmt.Sprintf("copied target=%s format=%s", target.Label, ext))
		} else {
			if err := p.uploadEncodedObject(ctx, event.Bucket, mainObjectName, contentTypeFromExt(ext), event.Generation, func(w io.Writer) error {
				return encodeByExtToWriter(w, targetImg, ext)
			}); err != nil {
				return fmt.Errorf("encode %s: %w", mainObjectName, err)
			}
			p.logMemory(event.Name, fmt.Sprintf("encoded and uploaded target=%s format=%s", target.Label, ext))
		}

		webpObjectName := baseDir + imageFileID + "-" + target.Label + ".webP"
		if err := p.uploadEncodedObject(ctx, event.Bucket, webpObjectName, "image/webp", event.Generation, func(w io.Writer) error {
			return encodeWebPToWriter(w, targetImg)
		}); err != nil {
			return fmt.Errorf("encode %s: %w", webpObjectName, err)
		}

		resized = nil
		targetImg = nil
		p.logMemory(event.Name, fmt.Sprintf("encoded and uploaded target=%s format=.webP", target.Label))
	}

	if err := p.uploadEncodedObject(ctx, event.Bucket, originalWebPName, "image/webp", event.Generation, func(w io.Writer) error {
		return encodeWebPToWriter(w, sourceImg)
	}); err != nil {
		log.Printf("failed to encode deferred original webp for %s: %v", event.Name, err)
	} else {
		p.logMemory(event.Name, "encoded and uploaded deferred original webp")
	}

	sourceImg = nil
	p.logMemory(event.Name, "done")
	return nil
}

func (p *Processor) logMemory(objectName, checkpoint string) {
	if !p.cfg.LogMemory {
		return
	}

	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	log.Printf(
		"memory checkpoint object=%s checkpoint=%q heap_alloc_mib=%d heap_sys_mib=%d heap_idle_mib=%d heap_released_mib=%d stack_inuse_mib=%d num_gc=%d",
		objectName,
		checkpoint,
		bytesToMiB(stats.HeapAlloc),
		bytesToMiB(stats.HeapSys),
		bytesToMiB(stats.HeapIdle),
		bytesToMiB(stats.HeapReleased),
		bytesToMiB(stats.StackInuse),
		stats.NumGC,
	)
}

func bytesToMiB(v uint64) uint64 {
	return v / 1024 / 1024
}

func (p *Processor) sourceObject(bucketName, objectName, sourceGeneration string) *storage.ObjectHandle {
	object := p.storage.Bucket(bucketName).Object(objectName)
	generation, err := strconv.ParseInt(sourceGeneration, 10, 64)
	if err != nil || generation <= 0 {
		return object
	}
	return object.Generation(generation)
}

func (p *Processor) alreadyProcessedSourceGeneration(ctx context.Context, bucketName, sentinelObjectName, sourceGeneration string) (bool, error) {
	if sourceGeneration == "" {
		return false, nil
	}

	attrs, err := p.storage.Bucket(bucketName).Object(sentinelObjectName).Attrs(ctx)
	if err != nil {
		if errors.Is(err, storage.ErrObjectNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("check sentinel object %s: %w", sentinelObjectName, err)
	}
	return attrs.Metadata[sourceGenerationMetadataKey] == sourceGeneration, nil
}

func isDerivedObjectName(name string) bool {
	if filepath.Ext(name) == ".webP" {
		return true
	}
	return trailingResizeLabelPattern.MatchString(strings.TrimSuffix(filepath.Base(name), filepath.Ext(name)))
}

func completionSentinelObjectName(baseDir, imageFileID, fallback string, targets []ResizeTarget) string {
	if len(targets) == 0 {
		return fallback
	}
	return baseDir + imageFileID + "-" + targets[len(targets)-1].Label + ".webP"
}

func (p *Processor) uploadEncodedObject(ctx context.Context, bucketName, objectName, contentType, sourceGeneration string, encode func(io.Writer) error) error {
	writer := p.storage.Bucket(bucketName).Object(objectName).NewWriter(ctx)
	writer.ContentType = contentType
	writer.CacheControl = p.cfg.CacheControl
	if sourceGeneration != "" {
		writer.Metadata = map[string]string{
			sourceGenerationMetadataKey: sourceGeneration,
		}
	}

	if err := encode(writer); err != nil {
		_ = writer.Close()
		return err
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("close object %s: %w", objectName, err)
	}

	log.Printf("uploaded gs://%s/%s", bucketName, objectName)
	return nil
}

func (p *Processor) copyObject(ctx context.Context, bucketName string, sourceObject *storage.ObjectHandle, objectName, contentType, sourceGeneration string) error {
	copier := p.storage.Bucket(bucketName).Object(objectName).CopierFrom(sourceObject)
	copier.ContentType = contentType
	copier.CacheControl = p.cfg.CacheControl
	if sourceGeneration != "" {
		copier.Metadata = map[string]string{
			sourceGenerationMetadataKey: sourceGeneration,
		}
	}
	if _, err := copier.Run(ctx); err != nil {
		return fmt.Errorf("copy object %s: %w", objectName, err)
	}

	log.Printf("copied gs://%s/%s", bucketName, objectName)
	return nil
}

func isSupportedImage(name string) bool {
	ext := filepath.Ext(name)
	if ext == ".webP" {
		return false
	}

	switch strings.ToLower(ext) {
	case ".jpg", ".jpeg", ".png", ".gif", ".tif", ".tiff", ".webp":
		return true
	default:
		return false
	}
}

func resizeImage(src image.Image, targetWidth int) *image.NRGBA {
	bounds := src.Bounds()
	width := bounds.Dx()
	height := bounds.Dy()
	if width == 0 || height == 0 || targetWidth <= 0 {
		return toNRGBA(src)
	}

	targetHeight := height * targetWidth / width
	if targetHeight <= 0 {
		targetHeight = 1
	}

	dst := image.NewNRGBA(image.Rect(0, 0, targetWidth, targetHeight))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, bounds, xdraw.Over, nil)
	return dst
}

func validateSourceImageSize(width, height, maxPixels int) error {
	if maxPixels <= 0 || width <= 0 || height <= 0 {
		return nil
	}
	pixels := int64(width) * int64(height)
	if pixels <= int64(maxPixels) {
		return nil
	}
	return fmt.Errorf("source image is too large: %dx%d=%d pixels exceeds max %d", width, height, pixels, maxPixels)
}

func applyWatermark(base *image.NRGBA, watermark *image.NRGBA, scale, marginRatio, opacity float64) *image.NRGBA {
	if base == nil || watermark == nil {
		return base
	}
	if scale <= 0 {
		scale = 0.15
	}
	if marginRatio < 0 {
		marginRatio = 0
	}
	if opacity <= 0 {
		opacity = 1
	}
	if opacity > 1 {
		opacity = 1
	}

	targetWidth := int(float64(base.Bounds().Dx()) * scale)
	if targetWidth < 1 {
		targetWidth = 1
	}

	scaled := resizeImage(watermark, targetWidth)
	if opacity < 1 {
		scaled = adjustOpacity(scaled, opacity)
	}

	margin := int(float64(base.Bounds().Dx()) * marginRatio)
	x := base.Bounds().Dx() - scaled.Bounds().Dx() - margin
	y := base.Bounds().Dy() - scaled.Bounds().Dy() - margin
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}

	result := cloneNRGBA(base)
	rect := image.Rect(x, y, x+scaled.Bounds().Dx(), y+scaled.Bounds().Dy())
	imagedraw.Draw(result, rect, scaled, image.Point{}, imagedraw.Over)
	return result
}

func adjustOpacity(img *image.NRGBA, opacity float64) *image.NRGBA {
	out := cloneNRGBA(img)
	for i := 3; i < len(out.Pix); i += 4 {
		out.Pix[i] = uint8(float64(out.Pix[i]) * opacity)
	}
	return out
}

func encodeByExt(img image.Image, ext string) ([]byte, error) {
	var buf bytes.Buffer
	if err := encodeByExtToWriter(&buf, img, ext); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func encodeByExtToWriter(w io.Writer, img image.Image, ext string) error {
	switch strings.ToLower(ext) {
	case ".jpg", ".jpeg":
		return jpeg.Encode(w, flattenIfNeeded(img), &jpeg.Options{Quality: 85})
	case ".png":
		return png.Encode(w, img)
	case ".gif":
		return gif.Encode(w, flattenIfNeeded(img), nil)
	case ".tif", ".tiff":
		return xtiff.Encode(w, img, nil)
	case ".webp":
		return encodeWebPToWriter(w, img)
	default:
		return jpeg.Encode(w, flattenIfNeeded(img), &jpeg.Options{Quality: 85})
	}
}

func applyEXIFOrientation(img image.Image, data []byte) image.Image {
	orientation := exifOrientation(data)
	switch orientation {
	case 3:
		return rotate180(img)
	case 6:
		return rotate90CW(img)
	case 8:
		return rotate90CCW(img)
	default:
		return img
	}
}

func exifOrientation(data []byte) int {
	exifData, err := exifpkg.Decode(bytes.NewReader(data))
	if err != nil {
		return 1
	}
	tag, err := exifData.Get(exifpkg.Orientation)
	if err != nil {
		return 1
	}
	orientation, err := tag.Int(0)
	if err != nil {
		return 1
	}
	return orientation
}

func rotate180(src image.Image) *image.NRGBA {
	img := toNRGBA(src)
	bounds := img.Bounds()
	width := bounds.Dx()
	height := bounds.Dy()
	dst := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			dst.Set(width-1-x, height-1-y, img.At(x, y))
		}
	}
	return dst
}

func rotate90CW(src image.Image) *image.NRGBA {
	img := toNRGBA(src)
	bounds := img.Bounds()
	width := bounds.Dx()
	height := bounds.Dy()
	dst := image.NewNRGBA(image.Rect(0, 0, height, width))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			dst.Set(height-1-y, x, img.At(x, y))
		}
	}
	return dst
}

func rotate90CCW(src image.Image) *image.NRGBA {
	img := toNRGBA(src)
	bounds := img.Bounds()
	width := bounds.Dx()
	height := bounds.Dy()
	dst := image.NewNRGBA(image.Rect(0, 0, height, width))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			dst.Set(y, width-1-x, img.At(x, y))
		}
	}
	return dst
}

func encodeWebP(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	if err := encodeWebPToWriter(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func encodeWebPToWriter(w io.Writer, img image.Image) error {
	return webp.Encode(w, img, webp.Options{
		Quality: 85,
		Method:  4,
	})
}

func flattenIfNeeded(img image.Image) image.Image {
	nrgba := toNRGBA(img)
	if !hasAlpha(nrgba) {
		return nrgba
	}
	rgba := image.NewRGBA(nrgba.Bounds())
	imagedraw.Draw(rgba, rgba.Bounds(), image.NewUniform(image.White), image.Point{}, imagedraw.Src)
	imagedraw.Draw(rgba, rgba.Bounds(), nrgba, image.Point{}, imagedraw.Over)
	return rgba
}

func hasAlpha(img *image.NRGBA) bool {
	for i := 3; i < len(img.Pix); i += 4 {
		if img.Pix[i] != 0xff {
			return true
		}
	}
	return false
}

func toNRGBA(src image.Image) *image.NRGBA {
	bounds := src.Bounds()
	dst := image.NewNRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	imagedraw.Draw(dst, dst.Bounds(), src, bounds.Min, imagedraw.Src)
	return dst
}

func cloneNRGBA(src *image.NRGBA) *image.NRGBA {
	dst := image.NewNRGBA(src.Bounds())
	copy(dst.Pix, src.Pix)
	return dst
}
