package main

import (
	"bytes"
	"context"
	"image"
	"image/jpeg"
	"io"
	"strconv"
	"strings"
	"testing"

	"github.com/fsouza/fake-gcs-server/fakestorage"
)

func jpegFixture(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 32, 32))
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 80}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestProcess_WithFakeGCS(t *testing.T) {
	jpg := jpegFixture(t)
	srv, err := fakestorage.NewServerWithOptions(fakestorage.Options{
		Scheme: "http",
		InitialObjects: []fakestorage.Object{
			{
				ObjectAttrs: fakestorage.ObjectAttrs{
					BucketName: "test-bucket",
					Name:       "images/pipe.jpg",
				},
				Content: jpg,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()

	client := srv.Client()
	cfg := Config{
		ResizeTargets:   []ResizeTarget{{Label: "w480", Width: 24}},
		EnableWatermark: false,
		CacheControl:    "public, max-age=1",
	}
	p, err := NewProcessor(cfg, client)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	err = p.Process(ctx, storageEvent{Bucket: "test-bucket", Name: "images/pipe.jpg"})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := client.Bucket("test-bucket").Object("images/pipe-w480.jpg").Attrs(ctx); err != nil {
		t.Fatal("expected resized output:", err)
	}
}

func TestProcess_ReturnsHardErrorWhenDecodeConfigFails(t *testing.T) {
	srv, err := fakestorage.NewServerWithOptions(fakestorage.Options{
		Scheme: "http",
		InitialObjects: []fakestorage.Object{
			{
				ObjectAttrs: fakestorage.ObjectAttrs{
					BucketName: "test-bucket",
					Name:       "images/broken.jpg",
				},
				Content: []byte("not an image"),
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()

	client := srv.Client()
	cfg := Config{
		ResizeTargets:   []ResizeTarget{{Label: "w480", Width: 24}},
		EnableWatermark: false,
		CacheControl:    "public, max-age=1",
		MaxSourcePixels: 30000000,
	}
	p, err := NewProcessor(cfg, client)
	if err != nil {
		t.Fatal(err)
	}

	err = p.Process(context.Background(), storageEvent{Bucket: "test-bucket", Name: "images/broken.jpg"})
	if err == nil {
		t.Fatal("expected decode config error")
	}
	if !strings.Contains(err.Error(), "decode image config") {
		t.Fatalf("expected decode config error, got %v", err)
	}
}

func TestProcess_CopiesSourceWhenTargetWouldUpsize(t *testing.T) {
	jpg := jpegFixture(t)
	srv, err := fakestorage.NewServerWithOptions(fakestorage.Options{
		Scheme: "http",
		InitialObjects: []fakestorage.Object{
			{
				ObjectAttrs: fakestorage.ObjectAttrs{
					BucketName: "test-bucket",
					Name:       "images/pipe.jpg",
				},
				Content: jpg,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()

	client := srv.Client()
	cfg := Config{
		ResizeTargets:   []ResizeTarget{{Label: "w64", Width: 64}},
		EnableWatermark: false,
		CacheControl:    "public, max-age=1",
	}
	p, err := NewProcessor(cfg, client)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	sourceAttrs, err := client.Bucket("test-bucket").Object("images/pipe.jpg").Attrs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sourceGeneration := strconv.FormatInt(sourceAttrs.Generation, 10)
	if err := p.Process(ctx, storageEvent{Bucket: "test-bucket", Name: "images/pipe.jpg", Generation: sourceGeneration}); err != nil {
		t.Fatal(err)
	}

	reader, err := client.Bucket("test-bucket").Object("images/pipe-w64.jpg").NewReader(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, jpg) {
		t.Fatal("expected upsize target to be copied from source")
	}
	attrs, err := client.Bucket("test-bucket").Object("images/pipe-w64.jpg").Attrs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if attrs.Metadata["sourceGeneration"] != sourceGeneration {
		t.Fatalf("expected sourceGeneration metadata, got %+v", attrs.Metadata)
	}
}

func TestProcess_SkipsDuplicateSourceGeneration(t *testing.T) {
	jpg := jpegFixture(t)
	srv, err := fakestorage.NewServerWithOptions(fakestorage.Options{
		Scheme: "http",
		InitialObjects: []fakestorage.Object{
			{
				ObjectAttrs: fakestorage.ObjectAttrs{
					BucketName: "test-bucket",
					Name:       "images/pipe.jpg",
				},
				Content: jpg,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()

	client := srv.Client()
	cfg := Config{
		ResizeTargets:   []ResizeTarget{{Label: "w480", Width: 24}},
		EnableWatermark: false,
		CacheControl:    "public, max-age=1",
	}
	p, err := NewProcessor(cfg, client)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	sourceAttrs, err := client.Bucket("test-bucket").Object("images/pipe.jpg").Attrs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	firstSourceGeneration := strconv.FormatInt(sourceAttrs.Generation, 10)
	if err := p.Process(ctx, storageEvent{Bucket: "test-bucket", Name: "images/pipe.jpg", Generation: firstSourceGeneration}); err != nil {
		t.Fatal(err)
	}

	firstAttrs, err := client.Bucket("test-bucket").Object("images/pipe-w480.jpg").Attrs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if firstAttrs.Metadata["sourceGeneration"] != firstSourceGeneration {
		t.Fatalf("expected sourceGeneration metadata, got %+v", firstAttrs.Metadata)
	}

	if err := client.Bucket("test-bucket").Object("images/pipe-w480.webP").Delete(ctx); err != nil {
		t.Fatal(err)
	}
	if err := p.Process(ctx, storageEvent{Bucket: "test-bucket", Name: "images/pipe.jpg", Generation: firstSourceGeneration}); err != nil {
		t.Fatal(err)
	}
	recoveredAttrs, err := client.Bucket("test-bucket").Object("images/pipe-w480.jpg").Attrs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if recoveredAttrs.Generation == firstAttrs.Generation {
		t.Fatal("missing completion sentinel should allow retry to rebuild outputs")
	}

	if err := p.Process(ctx, storageEvent{Bucket: "test-bucket", Name: "images/pipe.jpg", Generation: firstSourceGeneration}); err != nil {
		t.Fatal(err)
	}
	duplicateAttrs, err := client.Bucket("test-bucket").Object("images/pipe-w480.jpg").Attrs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if duplicateAttrs.Generation != recoveredAttrs.Generation {
		t.Fatalf("duplicate source generation should not rewrite output: recovered=%d duplicate=%d", recoveredAttrs.Generation, duplicateAttrs.Generation)
	}

	writer := client.Bucket("test-bucket").Object("images/pipe.jpg").NewWriter(ctx)
	if _, err := writer.Write(jpg); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	newSourceGeneration := strconv.FormatInt(writer.Attrs().Generation, 10)
	if err := p.Process(ctx, storageEvent{Bucket: "test-bucket", Name: "images/pipe.jpg", Generation: newSourceGeneration}); err != nil {
		t.Fatal(err)
	}
	newAttrs, err := client.Bucket("test-bucket").Object("images/pipe-w480.jpg").Attrs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if newAttrs.Generation == duplicateAttrs.Generation {
		t.Fatal("new source generation should rewrite output")
	}
	if newAttrs.Metadata["sourceGeneration"] != newSourceGeneration {
		t.Fatalf("expected updated sourceGeneration metadata, got %+v", newAttrs.Metadata)
	}
}
