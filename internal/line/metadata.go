package line

import (
	"howett.net/plist"
)

// metadata holds the entries of ZMESSAGE.ZCONTENTMETADATA the importer reads.
// The column is an NSKeyedArchiver dictionary of strings; entries not listed
// here (mentions, media descriptors, sticker ids) are ignored.
type metadata struct {
	ReplyTo    string // replyOriginalMessageId: the LINE message id this message answers
	SystemType string // systemMessageTypeName: set on notices LINE renders as system text
	AltText    string // ALT_TEXT: the readable text of a rich message
}

// decodeMetadata reads the entries it knows from a keyed archive. A blob it
// cannot read yields the zero value; the message is then imported as if it
// carried no metadata.
func decodeMetadata(blob []byte) metadata {
	var md metadata
	if len(blob) == 0 {
		return md
	}
	var archive struct {
		Top     map[string]plist.UID `plist:"$top"`
		Objects []any                `plist:"$objects"`
	}
	if _, err := plist.Unmarshal(blob, &archive); err != nil {
		return md
	}
	resolve := func(v any) any {
		if uid, ok := v.(plist.UID); ok && uid < plist.UID(len(archive.Objects)) {
			return archive.Objects[uid]
		}
		return v
	}
	root, ok := resolve(archive.Top["root"]).(map[string]any)
	if !ok {
		return md
	}
	keys, _ := root["NS.keys"].([]any)
	values, _ := root["NS.objects"].([]any)
	for i, k := range keys {
		if i >= len(values) {
			break
		}
		key, ok := resolve(k).(string)
		if !ok {
			continue
		}
		value, ok := resolve(values[i]).(string)
		if !ok {
			continue
		}
		switch key {
		case "replyOriginalMessageId":
			md.ReplyTo = value
		case "systemMessageTypeName":
			md.SystemType = value
		case "ALT_TEXT":
			md.AltText = value
		}
	}
	return md
}
