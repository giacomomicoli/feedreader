package transcript

// Defensive limits on YouTube's answers, which are treated as hostile like
// any fetched content. They are not tunable: each is far above what YouTube
// sends. Decoding allocates at most about the size of the answer (see
// memory_test.go); these bound the work. The length of the text itself is
// config.TranscriptMaxRunes.
const (
	// maxPlayerBytes bounds the player response that is decoded. A video
	// dubbed into twenty languages answers about 340 KB. A larger one is
	// ErrTooLarge.
	maxPlayerBytes = 4 << 20
	// maxCaptionBytes bounds the caption track that is decoded. Automatic
	// captions in JSON take about 1.2 MB per hour of speech; the fetch
	// size cap (10 MB by default) usually binds first. A track over either
	// is ErrTooLarge.
	maxCaptionBytes = 16 << 20
	// maxCaptionTracks bounds the caption tracks considered, and
	// maxAudioTracks the audio tracks: a video with automatic dubbing lists
	// about twenty of each. Tracks past the limit are ignored, and so are
	// indices pointing at them.
	maxCaptionTracks = 200
	maxAudioTracks   = 200
	// maxIndexDigits bounds the digits of an index into those lists: a
	// longer number points past them anyway, and nine digits fit an int
	// on every platform.
	maxIndexDigits = 9
	// maxCaptionURLBytes bounds the caption URL taken from the player
	// response; YouTube's are under 400 bytes.
	maxCaptionURLBytes = 2048
	// maxLangBytes bounds a track's language code, a BCP 47 tag such as
	// "en", "zh-Hant" or "es-419": RFC 5646 asks implementations to handle
	// tags of at least 35 characters.
	maxLangBytes = 35
	// maxStatusBytes bounds the playability status shown in an error
	// message, such as "LIVE_STREAM_OFFLINE".
	maxStatusBytes = 40
)

// Paragraphs. Caption tracks are a sequence of short cues; the text joins
// them with spaces and starts a new paragraph at a change of speaker or
// when the current one grows long, so a long transcript stays readable.
const (
	// paragraphSoftRunes: once a paragraph holds this many runes, the next
	// cue that follows the end of a sentence starts a new one.
	paragraphSoftRunes = 600
	// paragraphHardRunes: once a paragraph holds this many runes, the next
	// cue starts a new one even without punctuation (automatic captions in
	// some languages have none).
	paragraphHardRunes = 1500
)
