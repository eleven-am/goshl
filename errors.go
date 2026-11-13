package goshl

import (
	"errors"

	"github.com/eleven-am/goshl/internal/coordination"
	"github.com/eleven-am/goshl/internal/domain"
	"github.com/eleven-am/goshl/internal/hwaccel"
	"github.com/eleven-am/goshl/internal/source"
)

var (
	ErrInvalidOptions               = errors.New("invalid goshl options")
	ErrInvalidStreamType            = errors.New("invalid stream type")
	ErrRenditionNotFound            = errors.New("rendition not found")
	ErrSegmentOutOfRange            = errors.New("segment index out of range")
	ErrSegmentTimeout               = errors.New("segment wait timed out")
	ErrMetadataTimeout              = errors.New("metadata wait timed out")
	ErrCacheInvalidationUnsupported = errors.New("storage does not support cache invalidation")
	ErrInvalidPreviewConfig         = errors.New("invalid preview configuration")
	ErrInvalidThumbnailConfig       = errors.New("invalid thumbnail configuration")
)

var ErrEnqueueRejected = domain.ErrEnqueueRejected

var ErrTonemapUnavailable = hwaccel.ErrTonemapUnavailable

var ErrUnsupportedSource = source.ErrUnsupportedSource

var ErrControllerStopped = coordination.ErrStopped
