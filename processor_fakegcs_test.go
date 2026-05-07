package main

import (
	"bytes"
	"context"
	"image"
	"image/jpeg"
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
	if err := p.Process(ctx, storageEvent{Bucket: "test-bucket", Name: "images/pipe.jpg", Generation: "111"}); err != nil {
		t.Fatal(err)
	}

	firstAttrs, err := client.Bucket("test-bucket").Object("images/pipe-w480.jpg").Attrs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if firstAttrs.Metadata["sourceGeneration"] != "111" {
		t.Fatalf("expected sourceGeneration metadata, got %+v", firstAttrs.Metadata)
	}

	if err := client.Bucket("test-bucket").Object("images/pipe-w480.webP").Delete(ctx); err != nil {
		t.Fatal(err)
	}
	if err := p.Process(ctx, storageEvent{Bucket: "test-bucket", Name: "images/pipe.jpg", Generation: "111"}); err != nil {
		t.Fatal(err)
	}
	recoveredAttrs, err := client.Bucket("test-bucket").Object("images/pipe-w480.jpg").Attrs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if recoveredAttrs.Generation == firstAttrs.Generation {
		t.Fatal("missing completion sentinel should allow retry to rebuild outputs")
	}

	if err := p.Process(ctx, storageEvent{Bucket: "test-bucket", Name: "images/pipe.jpg", Generation: "111"}); err != nil {
		t.Fatal(err)
	}
	duplicateAttrs, err := client.Bucket("test-bucket").Object("images/pipe-w480.jpg").Attrs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if duplicateAttrs.Generation != recoveredAttrs.Generation {
		t.Fatalf("duplicate source generation should not rewrite output: recovered=%d duplicate=%d", recoveredAttrs.Generation, duplicateAttrs.Generation)
	}

	if err := p.Process(ctx, storageEvent{Bucket: "test-bucket", Name: "images/pipe.jpg", Generation: "222"}); err != nil {
		t.Fatal(err)
	}
	newAttrs, err := client.Bucket("test-bucket").Object("images/pipe-w480.jpg").Attrs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if newAttrs.Generation == duplicateAttrs.Generation {
		t.Fatal("new source generation should rewrite output")
	}
	if newAttrs.Metadata["sourceGeneration"] != "222" {
		t.Fatalf("expected updated sourceGeneration metadata, got %+v", newAttrs.Metadata)
	}
}
