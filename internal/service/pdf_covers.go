package service

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultCoverWidth   = 320
	defaultCoverQuality = 80
	defaultCoverWorkers = 2
	defaultCoverTimeout = 30 * time.Second
	defaultFailureTTL   = 5 * time.Minute
	defaultPDFRenderer  = "pdftoppm"
)

// PDFCoverConfig configures on-demand PDF thumbnail rendering.
type PDFCoverConfig struct {
	BookRoot   string
	CacheDir   string
	Command    string
	Width      int
	Quality    int
	Workers    int
	Timeout    time.Duration
	FailureTTL time.Duration
}

type coverLock struct {
	mutex sync.Mutex
	refs  int
}

type coverFailure struct {
	expires time.Time
	size    int64
	mtime   int64
}

// PDFCovers renders and caches PDF thumbnails.
type PDFCovers struct {
	bookRoot   string
	cacheDir   string
	command    string
	width      int
	quality    int
	timeout    time.Duration
	failureTTL time.Duration
	slots      chan struct{}
	locksMu    sync.Mutex
	locks      map[string]*coverLock
	failuresMu sync.Mutex
	failures   map[string]coverFailure
	renderer   func(context.Context, string, string) error
}

// NewPDFCovers creates a PDF thumbnail cache.
func NewPDFCovers(config PDFCoverConfig) (*PDFCovers, error) {
	if config.BookRoot == "" {
		return nil, fmt.Errorf("book root is required for PDF covers")
	}
	if config.CacheDir == "" {
		return nil, fmt.Errorf("PDF cover cache directory is required")
	}
	if config.Command == "" {
		config.Command = defaultPDFRenderer
	}
	if config.Width <= 0 {
		config.Width = defaultCoverWidth
	}
	if config.Quality <= 0 || config.Quality > 100 {
		config.Quality = defaultCoverQuality
	}
	if config.Workers <= 0 {
		config.Workers = defaultCoverWorkers
	}
	if config.Timeout <= 0 {
		config.Timeout = defaultCoverTimeout
	}
	if config.FailureTTL <= 0 {
		config.FailureTTL = defaultFailureTTL
	}

	command, err := exec.LookPath(config.Command)
	if err != nil {
		return nil, fmt.Errorf("find PDF renderer %q: %w", config.Command, err)
	}
	bookRoot, err := filepath.Abs(config.BookRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve book root: %w", err)
	}
	cacheDir, err := filepath.Abs(config.CacheDir)
	if err != nil {
		return nil, fmt.Errorf("resolve PDF cover cache: %w", err)
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return nil, fmt.Errorf("create PDF cover cache: %w", err)
	}
	cacheRelative, err := filepath.Rel(bookRoot, cacheDir)
	if err != nil {
		return nil, fmt.Errorf("compare PDF cover cache and book root: %w", err)
	}
	if cacheRelative == "." || (cacheRelative != ".." && !strings.HasPrefix(cacheRelative, ".."+string(filepath.Separator))) {
		return nil, fmt.Errorf("PDF cover cache must be outside the book root")
	}

	profile := fmt.Sprintf("w%d-q%d", config.Width, config.Quality)
	covers := &PDFCovers{
		bookRoot:   bookRoot,
		cacheDir:   filepath.Join(cacheDir, profile),
		command:    command,
		width:      config.Width,
		quality:    config.Quality,
		timeout:    config.Timeout,
		failureTTL: config.FailureTTL,
		slots:      make(chan struct{}, config.Workers),
		locks:      make(map[string]*coverLock),
		failures:   make(map[string]coverFailure),
	}
	covers.renderer = covers.renderPage
	return covers, nil
}

func (c *PDFCovers) render(ctx context.Context, source string) (string, error) {
	relative, err := filepath.Rel(c.bookRoot, source)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("PDF is outside the book root")
	}

	destination := filepath.Join(c.cacheDir, relative+".jpg")
	unlock := c.lock(destination)
	defer unlock()

	sourceInfo, err := os.Stat(source)
	if err != nil {
		return "", fmt.Errorf("read PDF information: %w", err)
	}
	valid, err := validCover(sourceInfo, destination)
	if err != nil {
		return "", err
	}
	if valid {
		return destination, nil
	}
	if c.recentFailure(destination, sourceInfo) {
		return "", fmt.Errorf("PDF cover render failed recently")
	}

	select {
	case c.slots <- struct{}{}:
		defer func() { <-c.slots }()
	case <-ctx.Done():
		return "", ctx.Err()
	}

	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return "", fmt.Errorf("create PDF cover directory: %w", err)
	}

	temp, err := os.CreateTemp(filepath.Dir(destination), ".pdf-cover-*")
	if err != nil {
		return "", fmt.Errorf("create temporary PDF cover: %w", err)
	}
	tempRoot := temp.Name()
	if err := temp.Close(); err != nil {
		return "", fmt.Errorf("close temporary PDF cover: %w", err)
	}
	if err := os.Remove(tempRoot); err != nil {
		return "", fmt.Errorf("prepare temporary PDF cover: %w", err)
	}
	tempImage := tempRoot + ".jpg"
	defer os.Remove(tempImage)

	if err := c.renderer(ctx, source, tempRoot); err != nil {
		c.recordFailure(destination, sourceInfo)
		return "", err
	}
	if err := replaceFile(tempImage, destination); err != nil {
		return "", fmt.Errorf("store PDF cover: %w", err)
	}
	if err := os.Chtimes(destination, time.Now(), sourceInfo.ModTime()); err != nil {
		return "", fmt.Errorf("set PDF cover modification time: %w", err)
	}
	if err := writeCoverMetadata(destination+".meta", sourceInfo); err != nil {
		return "", err
	}
	c.clearFailure(destination)

	return destination, nil
}

func (c *PDFCovers) renderPage(ctx context.Context, source, outputRoot string) error {
	renderCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	args := []string{
		"-f", "1",
		"-l", "1",
		"-singlefile",
		"-scale-to-x", strconv.Itoa(c.width),
		"-scale-to-y", "-1",
		"-jpeg",
		"-jpegopt", "quality=" + strconv.Itoa(c.quality),
		source,
		outputRoot,
	}
	if output, err := exec.CommandContext(renderCtx, c.command, args...).CombinedOutput(); err != nil {
		return fmt.Errorf("render PDF cover: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func validCover(sourceInfo os.FileInfo, destination string) (bool, error) {
	coverInfo, err := os.Stat(destination)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read PDF cover information: %w", err)
	}

	metadata, err := os.ReadFile(destination + ".meta")
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read PDF cover metadata: %w", err)
	}

	return coverInfo.Size() > 0 && string(metadata) == coverMetadata(sourceInfo), nil
}

func coverMetadata(info os.FileInfo) string {
	return fmt.Sprintf("%d %d\n", info.Size(), info.ModTime().UnixNano())
}

func writeCoverMetadata(destination string, sourceInfo os.FileInfo) error {
	temp, err := os.CreateTemp(filepath.Dir(destination), ".pdf-cover-meta-*")
	if err != nil {
		return fmt.Errorf("create PDF cover metadata: %w", err)
	}
	tempName := temp.Name()
	defer os.Remove(tempName)

	if _, err := temp.WriteString(coverMetadata(sourceInfo)); err != nil {
		temp.Close()
		return fmt.Errorf("write PDF cover metadata: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close PDF cover metadata: %w", err)
	}
	if err := replaceFile(tempName, destination); err != nil {
		return fmt.Errorf("store PDF cover metadata: %w", err)
	}
	return nil
}

func replaceFile(source, destination string) error {
	if err := os.Rename(source, destination); err == nil {
		return nil
	}
	if err := os.Remove(destination); err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.Rename(source, destination)
}

func (c *PDFCovers) lock(key string) func() {
	c.locksMu.Lock()
	if c.locks == nil {
		c.locks = make(map[string]*coverLock)
	}
	lock := c.locks[key]
	if lock == nil {
		lock = &coverLock{}
		c.locks[key] = lock
	}
	lock.refs++
	c.locksMu.Unlock()

	lock.mutex.Lock()
	return func() {
		lock.mutex.Unlock()
		c.locksMu.Lock()
		lock.refs--
		if lock.refs == 0 {
			delete(c.locks, key)
		}
		c.locksMu.Unlock()
	}
}

func (c *PDFCovers) recentFailure(key string, sourceInfo os.FileInfo) bool {
	c.failuresMu.Lock()
	defer c.failuresMu.Unlock()

	failure, found := c.failures[key]
	if !found {
		return false
	}
	if time.Now().After(failure.expires) || failure.size != sourceInfo.Size() || failure.mtime != sourceInfo.ModTime().UnixNano() {
		delete(c.failures, key)
		return false
	}
	return true
}

func (c *PDFCovers) recordFailure(key string, sourceInfo os.FileInfo) {
	c.failuresMu.Lock()
	if c.failures == nil {
		c.failures = make(map[string]coverFailure)
	}
	c.failures[key] = coverFailure{
		expires: time.Now().Add(c.failureTTL),
		size:    sourceInfo.Size(),
		mtime:   sourceInfo.ModTime().UnixNano(),
	}
	c.failuresMu.Unlock()
}

func (c *PDFCovers) clearFailure(key string) {
	c.failuresMu.Lock()
	delete(c.failures, key)
	c.failuresMu.Unlock()
}
