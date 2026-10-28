package email

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// Run reads the mailbox: a catch-up sync of every folder, then IDLE (or a
// pause) until there is more, again and again.
func (c *conn) Run(ctx context.Context) error {
	// New mail while idling arrives as an EXISTS; it only wakes the loop,
	// which then reads the folder itself.
	wake := make(chan struct{}, 1)
	cl, err := c.dialIMAP(&imapclient.Options{
		UnilateralDataHandler: &imapclient.UnilateralDataHandler{
			Mailbox: func(d *imapclient.UnilateralDataMailbox) {
				if d.NumMessages != nil {
					select {
					case wake <- struct{}{}:
					default:
					}
				}
			},
		},
	})
	if err != nil {
		return err
	}
	defer cl.Close()
	stop := context.AfterFunc(ctx, func() { cl.Close() })
	defer stop()
	fail := func(err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	if err := cl.Login(c.cfg.Username, c.cfg.Password).Wait(); err != nil {
		return fail(fmt.Errorf("log in to %s: %w", c.cfg.IMAP, err))
	}
	if _, err := cl.Select(c.cfg.Folder, &imap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
		return fail(fmt.Errorf("open %s: %w", c.cfg.Folder, err))
	}
	c.h.Connected(ctx)
	idle := cl.Caps().Has(imap.CapIdle)
	for {
		for {
			more, err := c.sync(ctx, cl)
			if err != nil {
				return fail(err)
			}
			if !more {
				break
			}
		}
		select {
		case <-wake:
		default:
		}
		if err := c.wait(ctx, cl, idle, wake); err != nil {
			return fail(err)
		}
	}
}

// wait returns when there may be new mail: on an EXISTS while idling, or
// after a while. The sent folder is never idled on, so it is polled.
func (c *conn) wait(ctx context.Context, cl *imapclient.Client, idle bool, wake <-chan struct{}) error {
	if !idle {
		t := time.NewTimer(pollEvery)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-cl.Closed():
			return fmt.Errorf("%s closed the connection", c.cfg.IMAP)
		case <-t.C:
			return nil
		}
	}
	d := idleFor
	if c.cfg.SentFolder != "" {
		d = pollEvery
	}
	t := time.NewTimer(d)
	defer t.Stop()
	cmd, err := cl.Idle()
	if err != nil {
		return err
	}
	select {
	case <-ctx.Done():
	case <-cl.Closed():
	case <-wake:
	case <-t.C:
	}
	if err := cmd.Close(); err != nil {
		return err
	}
	return cmd.Wait()
}

// sync reads what is new in every folder and passes it on, oldest first.
// More says a folder had more than one batch.
func (c *conn) sync(ctx context.Context, cl *imapclient.Client) (more bool, err error) {
	// The main folder goes last, so it stays selected for IDLE.
	var folders []string
	if c.cfg.SentFolder != "" && c.cfg.SentFolder != c.cfg.Folder {
		folders = append(folders, c.cfg.SentFolder)
	}
	folders = append(folders, c.cfg.Folder)
	var all []*message
	next := map[string]position{}
	for _, f := range folders {
		msgs, pos, m, err := c.syncFolder(cl, f)
		if err != nil {
			return false, err
		}
		all = append(all, msgs...)
		next[f] = pos
		more = more || m
	}
	// Mail from both folders interleaves into the same chats.
	sort.SliceStable(all, func(i, j int) bool { return all[i].Date.Before(all[j].Date) })
	selected := c.cfg.Folder
	for _, m := range all {
		if m.Cut && m.Size <= maxMail {
			// Big mail is read whole one at a time, for its files.
			whole, err := c.whole(cl, &selected, m)
			if err != nil {
				return false, err
			}
			m = whole
		}
		in, ok := c.incoming(m)
		if !ok {
			continue
		}
		if err := c.h.Receive(ctx, in); err != nil {
			return false, err
		}
		c.noteThread(in.Chat.ID, thread{MessageID: m.ID, Subject: m.Subject, At: m.Date})
	}
	if selected != c.cfg.Folder {
		// IDLE watches the selected folder.
		if _, err := cl.Select(c.cfg.Folder, &imap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
			return false, fmt.Errorf("open %s: %w", c.cfg.Folder, err)
		}
	}
	dirty := len(all) > 0
	for f, p := range next {
		dirty = dirty || c.position(f) != p
	}
	if !dirty {
		return more, nil
	}
	c.setPositions(next)
	return more, c.save(ctx)
}

// syncFolder reads the messages of a folder after its position: on the
// first sync, or after the server renumbered it, the last backfillMax of
// the last backfillAge.
func (c *conn) syncFolder(cl *imapclient.Client, folder string) ([]*message, position, bool, error) {
	pos := c.position(folder)
	sel, err := cl.Select(folder, &imap.SelectOptions{ReadOnly: true}).Wait()
	if err != nil {
		return nil, pos, false, fmt.Errorf("open %s: %w", folder, err)
	}
	fresh := pos.Validity != sel.UIDValidity
	if fresh {
		pos = position{Validity: sel.UIDValidity}
		if sel.UIDNext > 0 {
			// Where to go on from if nothing recent is there.
			pos.UID = uint32(sel.UIDNext) - 1
		}
	} else if sel.UIDNext > 0 && uint32(sel.UIDNext) <= pos.UID+1 {
		return nil, pos, false, nil
	}
	if sel.NumMessages == 0 {
		return nil, pos, false, nil
	}
	// Mail older than the backfill window is never read, even when the
	// position is somehow behind.
	criteria := &imap.SearchCriteria{Since: time.Now().Add(-backfillAge)}
	if !fresh {
		criteria.UID = []imap.UIDSet{{imap.UIDRange{Start: imap.UID(pos.UID + 1), Stop: 0}}}
	}
	res, err := cl.UIDSearch(criteria, nil).Wait()
	if err != nil {
		return nil, pos, false, fmt.Errorf("search %s: %w", folder, err)
	}
	var uids []imap.UID
	for _, u := range res.AllUIDs() {
		// "n:*" always matches the last message, even below n.
		if fresh || uint32(u) > pos.UID {
			uids = append(uids, u)
		}
	}
	slices.Sort(uids)
	more := false
	if fresh {
		if len(uids) > backfillMax {
			uids = uids[len(uids)-backfillMax:]
		}
	} else if len(uids) > batch {
		uids, more = uids[:batch], true
	}
	if len(uids) == 0 {
		return nil, pos, false, nil
	}
	bufs, err := fetchBodies(cl, uids)
	if err != nil {
		return nil, pos, false, fmt.Errorf("fetch from %s: %w", folder, err)
	}
	var msgs []*message
	for _, b := range bufs {
		if uint32(b.UID) > pos.UID {
			pos.UID = uint32(b.UID)
		}
		if len(b.BodySection) == 0 {
			continue
		}
		cut := b.RFC822Size > maxBody
		m, err := parse(b.BodySection[0].Bytes, !cut)
		if err != nil {
			// A message that cannot be read is skipped, not retried.
			continue
		}
		if m.Date.IsZero() {
			m.Date = b.InternalDate
		}
		m.Sent = folder == c.cfg.SentFolder
		m.Cut, m.Size, m.UID, m.Folder = cut, b.RFC822Size, b.UID, folder
		msgs = append(msgs, m)
	}
	if u := uint32(uids[len(uids)-1]); u > pos.UID {
		pos.UID = u
	}
	return msgs, pos, more, nil
}

// whole reads all of a message that was cut short, with its files. One
// that is gone, or changed under its UID, stays as it was.
func (c *conn) whole(cl *imapclient.Client, selected *string, m *message) (*message, error) {
	if *selected != m.Folder {
		if _, err := cl.Select(m.Folder, &imap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
			return nil, fmt.Errorf("open %s: %w", m.Folder, err)
		}
		*selected = m.Folder
	}
	bufs, err := cl.Fetch(imap.UIDSetNum(m.UID), &imap.FetchOptions{
		UID: true, BodySection: []*imap.FetchItemBodySection{{Peek: true}},
	}).Collect()
	if err != nil {
		return nil, fmt.Errorf("fetch from %s: %w", m.Folder, err)
	}
	if len(bufs) == 0 || len(bufs[0].BodySection) == 0 {
		return m, nil
	}
	w, err := parse(bufs[0].BodySection[0].Bytes, true)
	// Without a Message-ID, the key was made from the part read first.
	made := strings.HasSuffix(m.ID, "@say.invalid")
	if err != nil || w.ID != m.ID && !made {
		return m, nil
	}
	w.ID, w.Date, w.Sent, w.Folder = m.ID, m.Date, m.Sent, m.Folder
	return w, nil
}

// fetchBodies reads messages, only the first maxBody bytes of big ones.
// Small ones are read whole: some servers get partial fetches wrong.
func fetchBodies(cl *imapclient.Client, uids []imap.UID) ([]*imapclient.FetchMessageBuffer, error) {
	sizes, err := cl.Fetch(imap.UIDSetNum(uids...), &imap.FetchOptions{UID: true, RFC822Size: true}).Collect()
	if err != nil {
		return nil, err
	}
	var small, big imap.UIDSet
	for _, b := range sizes {
		if b.RFC822Size > maxBody {
			big.AddNum(b.UID)
		} else {
			small.AddNum(b.UID)
		}
	}
	var out []*imapclient.FetchMessageBuffer
	for _, set := range []struct {
		uids    imap.UIDSet
		partial *imap.SectionPartial
	}{{small, nil}, {big, &imap.SectionPartial{Size: maxBody}}} {
		if len(set.uids) == 0 {
			continue
		}
		bufs, err := cl.Fetch(set.uids, &imap.FetchOptions{
			UID: true, InternalDate: true, RFC822Size: true,
			BodySection: []*imap.FetchItemBodySection{{Peek: true, Partial: set.partial}},
		}).Collect()
		if err != nil {
			return nil, err
		}
		out = append(out, bufs...)
	}
	return out, nil
}

// dialIMAP connects to the IMAP server with the configured security.
func (c *conn) dialIMAP(opts *imapclient.Options) (*imapclient.Client, error) {
	host, _, _ := net.SplitHostPort(c.cfg.IMAP)
	opts.TLSConfig = &tls.Config{ServerName: host}
	opts.Dialer = &net.Dialer{Timeout: dialTimeout}
	var (
		cl  *imapclient.Client
		err error
	)
	switch {
	case c.cfg.Security == "none":
		cl, err = imapclient.DialInsecure(c.cfg.IMAP, opts)
	case c.startTLS(c.cfg.IMAP):
		cl, err = imapclient.DialStartTLS(c.cfg.IMAP, opts)
	default:
		cl, err = imapclient.DialTLS(c.cfg.IMAP, opts)
	}
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", c.cfg.IMAP, err)
	}
	return cl, nil
}
