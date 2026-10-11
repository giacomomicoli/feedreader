package innertube

// Defensive limits on the answers the YouTube fallback reads (browse.go,
// video.go), which are treated as hostile like any fetched content. They
// are not tunable. Each answer is refused whole above its cap, which the
// fetch also applies while reading it (fetch.Request.MaxBytes), so that a
// larger or compression-bomb answer is never held in memory; each field
// is cut to its cap while it is decoded.
const (
	// MaxBrowseBytes bounds a browse answer. A channel's Videos tab answers
	// about 430 KB, its Shorts tab up to about 400 KB; the Home tab,
	// which YouTube sends when the tab asked for does not exist, up to
	// about 1.6 MB.
	MaxBrowseBytes = 2 << 20
	// MaxVideoBytes bounds a player answer read for a video's details as
	// the web client (Web): about 10 KB.
	MaxVideoBytes = 256 << 10
	// MaxTitleBytes bounds a video's title: YouTube allows 100
	// characters.
	MaxTitleBytes = 4 << 10
	// MaxDescriptionBytes bounds a video's description: YouTube allows
	// 5000 characters, at most 20 KB in UTF-8.
	MaxDescriptionBytes = 32 << 10
	// MaxAuthorBytes bounds a channel's name.
	MaxAuthorBytes = 1 << 10
	// MaxReasonBytes bounds the playability reason read to tell a refusal
	// of this client from a video that does not play (see Classify). It
	// is never stored or shown.
	MaxReasonBytes = 1 << 10
	// MaxScalarBytes bounds the other strings read: ids, the publication
	// date, statuses.
	MaxScalarBytes = 64
)
