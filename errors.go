package goshl

import (
	"github.com/eleven-am/goshl/internal/coordination"
	"github.com/eleven-am/goshl/internal/domain"
	"github.com/eleven-am/goshl/internal/engine"
	"github.com/eleven-am/goshl/internal/hwaccel"
	"github.com/eleven-am/goshl/internal/source"
)

var (
	ErrInvalidOptions               = engine.ErrInvalidOptions
	ErrInvalidStreamType            = engine.ErrInvalidStreamType
	ErrRenditionNotFound            = engine.ErrRenditionNotFound
	ErrSegmentOutOfRange            = engine.ErrSegmentOutOfRange
	ErrSegmentTimeout               = engine.ErrSegmentTimeout
	ErrMetadataTimeout              = engine.ErrMetadataTimeout
	ErrCacheInvalidationUnsupported = engine.ErrCacheInvalidationUnsupported
	ErrInvalidPreviewConfig         = engine.ErrInvalidPreviewConfig
	ErrInvalidThumbnailConfig       = engine.ErrInvalidThumbnailConfig
)

var ErrEnqueueRejected = domain.ErrEnqueueRejected

var ErrTonemapUnavailable = hwaccel.ErrTonemapUnavailable

var ErrUnsupportedSource = source.ErrUnsupportedSource

var ErrControllerStopped = coordination.ErrStopped
