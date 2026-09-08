package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPDFCoverCache(t *testing.T) {
	bookRoot := t.TempDir()
	cacheRoot := t.TempDir()
	source := filepath.Join(bookRoot, "topic", "book.pdf")
	require.NoError(t, os.MkdirAll(filepath.Dir(source), 0o755))
	require.NoError(t, os.WriteFile(source, []byte("PDF"), 0o644))

	renders := 0
	covers := &PDFCovers{
		bookRoot: bookRoot,
		cacheDir: filepath.Join(cacheRoot, "w320-q80"),
		slots:    make(chan struct{}, 1),
		renderer: func(_ context.Context, _, outputRoot string) error {
			renders++
			return os.WriteFile(outputRoot+".jpg", []byte("JPEG"), 0o644)
		},
	}

	first, err := covers.render(context.Background(), source)
	require.NoError(t, err)
	second, err := covers.render(context.Background(), source)
	require.NoError(t, err)

	assert.Equal(t, first, second)
	assert.Equal(t, filepath.Join(cacheRoot, "w320-q80", "topic", "book.pdf.jpg"), first)
	assert.Equal(t, 1, renders)
	_, err = os.Stat(first + ".meta")
	require.NoError(t, err)

	future := time.Now().Add(time.Hour)
	require.NoError(t, os.Chtimes(source, future, future))
	_, err = covers.render(context.Background(), source)
	require.NoError(t, err)
	assert.Equal(t, 2, renders)
}

func TestPDFCoverFailureCache(t *testing.T) {
	bookRoot := t.TempDir()
	source := filepath.Join(bookRoot, "book.pdf")
	require.NoError(t, os.WriteFile(source, []byte("PDF"), 0o644))

	renders := 0
	covers := &PDFCovers{
		bookRoot:   bookRoot,
		cacheDir:   t.TempDir(),
		failureTTL: time.Minute,
		slots:      make(chan struct{}, 1),
		renderer: func(context.Context, string, string) error {
			renders++
			return errors.New("invalid PDF")
		},
	}

	_, err := covers.render(context.Background(), source)
	require.Error(t, err)
	_, err = covers.render(context.Background(), source)
	require.Error(t, err)
	assert.Equal(t, 1, renders)

	future := time.Now().Add(time.Hour)
	require.NoError(t, os.Chtimes(source, future, future))
	_, err = covers.render(context.Background(), source)
	require.Error(t, err)
	assert.Equal(t, 2, renders)
}

func TestPDFCoverLink(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "book.pdf"), []byte("PDF"), 0o644))

	s := OPDS{
		TrustedRoot: root,
		PDFCovers:   &PDFCovers{},
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)

	err := s.Handler(w, req)
	require.NoError(t, err)
	body := w.Body.String()
	assert.Contains(t, body, `rel="http://opds-spec.org/image/thumbnail"`)
	assert.Contains(t, body, `href="/cover?file=%2Fbook.pdf"`)
	assert.Contains(t, body, `type="image/jpeg"`)
}
