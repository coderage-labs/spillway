package proxy

// account_uuid rewrite — design doc §4, allowed mutation #2. Claude Code
// embeds the logged-in account's UUID inside `metadata.user_id` (a
// stringified JSON value) of /v1/messages; under rotation that disagrees with
// the injected token. Same-length, byte-exact patch — semantics ported from
// The account-uuid rewrite rule: only the 36-char
// account_uuid value INSIDE the metadata.user_id string is touched; a stray
// account_uuid elsewhere (user content, tool results) never is.

// uuidPrefix is the byte sequence `account_uuid":"` as it appears INSIDE the
// (escaped) user_id string: account_uuid \ " : \ "
var uuidPrefix = []byte(`account_uuid\":\"`)

type uuidFrame struct {
	container   byte // 'o' obj, 'a' arr
	name        string
	key         string
	awaitingKey bool
}

type uuidPatcher struct {
	newUUID []byte // nil unless exactly 36 bytes
	frames  []uuidFrame

	inStr      bool
	esc        bool
	readingKey bool
	keyBuf     []byte

	target        bool // inside the metadata.user_id string value
	matchPos      int  // uuidPrefix match progress (within target)
	uuidRemaining int  // value bytes left to overwrite
	done          bool // patched the one account_uuid already
	changed       bool
}

// patchAccountUUID returns body with the account_uuid inside metadata.user_id
// overwritten by newUUID. Returns body unchanged (same slice) when newUUID is
// not 36 bytes, when metadata/user_id/account_uuid is absent, or when the
// structure is malformed — pass through rather than guess.
func patchAccountUUID(body []byte, newUUID string) []byte {
	var p uuidPatcher
	if len(newUUID) == 36 {
		p.newUUID = []byte(newUUID)
	}
	out := p.push(body)
	if p.changed {
		return out
	}
	return body
}

func (p *uuidPatcher) push(chunk []byte) []byte {
	if p.newUUID == nil || p.done {
		return chunk
	}
	out := make([]byte, len(chunk))
	copy(out, chunk)
	for i := range out {
		out[i] = p.byte(out[i])
		if p.done {
			break
		}
	}
	return out
}

func (p *uuidPatcher) top() *uuidFrame {
	if len(p.frames) == 0 {
		return nil
	}
	return &p.frames[len(p.frames)-1]
}

func (p *uuidPatcher) byte(b byte) byte {
	if p.target {
		return p.targetByte(b)
	}

	if p.inStr {
		if p.esc {
			p.esc = false
			if p.readingKey {
				p.keyBuf = append(p.keyBuf, b)
			}
			return b
		}
		if b == '\\' {
			p.esc = true
			return b
		}
		if b == '"' { // end of string
			p.inStr = false
			if p.readingKey {
				if top := p.top(); top != nil {
					top.key = string(p.keyBuf)
				}
				p.keyBuf = p.keyBuf[:0]
				p.readingKey = false
			}
			return b
		}
		if p.readingKey {
			p.keyBuf = append(p.keyBuf, b)
		}
		return b
	}

	top := p.top()
	switch b {
	case '{':
		name := ""
		if top != nil {
			name = top.key
		}
		p.frames = append(p.frames, uuidFrame{container: 'o', name: name, awaitingKey: true})
	case '[':
		name := ""
		if top != nil {
			name = top.key
		}
		p.frames = append(p.frames, uuidFrame{container: 'a', name: name})
	case '}', ']':
		if len(p.frames) > 0 {
			p.frames = p.frames[:len(p.frames)-1]
		}
	case ':':
		if top != nil {
			top.awaitingKey = false
		}
	case ',':
		if top != nil && top.container == 'o' {
			top.awaitingKey = true
		}
	case '"':
		if top != nil && top.container == 'o' && top.awaitingKey {
			p.readingKey = true
			p.keyBuf = p.keyBuf[:0]
			p.inStr = true
			p.esc = false
		} else {
			p.inStr = true
			p.esc = false
			p.readingKey = false
			if top != nil && top.container == 'o' && top.name == "metadata" && top.key == "user_id" && len(p.frames) == 2 {
				p.target = true
				p.matchPos = 0
				p.uuidRemaining = 0
			}
		}
	}
	return b
}

// targetByte handles bytes inside the metadata.user_id string value: match
// the account_uuid key, overwrite its 36-byte value, exit on the unescaped
// closing quote.
func (p *uuidPatcher) targetByte(b byte) byte {
	if p.uuidRemaining > 0 {
		outByte := p.newUUID[len(p.newUUID)-p.uuidRemaining]
		p.uuidRemaining--
		if outByte != b {
			p.changed = true
		}
		if p.uuidRemaining == 0 {
			p.done = true // only one account_uuid per body
		}
		return outByte
	}
	if p.esc {
		p.esc = false
		p.match(b)
		return b
	}
	if b == '\\' {
		p.esc = true
		p.match(b)
		return b
	}
	if b == '"' { // end of user_id value
		p.target = false
		p.matchPos = 0
		return b
	}
	p.match(b)
	return b
}

func (p *uuidPatcher) match(b byte) {
	if b == uuidPrefix[p.matchPos] {
		p.matchPos++
		if p.matchPos == len(uuidPrefix) {
			p.uuidRemaining = 36
			p.matchPos = 0
		}
	} else {
		if b == uuidPrefix[0] {
			p.matchPos = 1 // prefix has no internal repeat of its first byte
		} else {
			p.matchPos = 0
		}
	}
}
