package telegram

import (
	"time"

	"github.com/authapon/jannyq/internal/channel"
)

// albumWait is how long after the first picture of an album the channel waits
// for the others: Telegram delivers them as separate messages.
const albumWait = 1200 * time.Millisecond

type albumKey struct {
	chat int64
	id   string
}

type album struct {
	in     channel.Incoming
	done   chan struct{}
	sealed bool
}

func (c *Channel) startAlbum(key albumKey, in channel.Incoming) *album {
	a := &album{in: in, done: make(chan struct{})}
	c.albumMu.Lock()
	if c.albums == nil {
		c.albums = map[albumKey]*album{}
	}
	c.albums[key] = a
	c.albumMu.Unlock()
	time.AfterFunc(albumWait, func() { close(a.done) })
	return a
}

// joinAlbum adds a message to an album that is still collecting, and reports
// whether there was one.
func (c *Channel) joinAlbum(key albumKey, in channel.Incoming) bool {
	c.albumMu.Lock()
	defer c.albumMu.Unlock()
	a := c.albums[key]
	if a == nil || a.sealed {
		return false
	}
	a.in.Attachments = append(a.in.Attachments, in.Attachments...)
	if a.in.Text == "" {
		a.in.Text = in.Text // the caption sits on one picture only
	}
	a.in.Addressed = a.in.Addressed || in.Addressed
	return true
}

// sealAlbum closes the album to further pictures and returns the message.
func (c *Channel) sealAlbum(key albumKey, a *album) channel.Incoming {
	c.albumMu.Lock()
	defer c.albumMu.Unlock()
	a.sealed = true
	if c.albums[key] == a {
		delete(c.albums, key)
	}
	return a.in
}
