package plainforge

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	serviceUpload  = "git-upload-pack"
	serviceReceive = "git-receive-pack"

	maxAdvBytes    = 1 << 20
	maxStatusBytes = 64 << 10
	maxResponseCap = 20 << 20
	maxTreeDepth   = 32
)

// HTTP is a Git smart HTTP client for the hub repository in Plainforge.
//
// It caches only the current tip commit of refs/heads/main, the directory
// trees reachable from that commit, and the file blob Chartroom asked for,
// so steady-state reads and writes never pull repository history again and
// memory stays bounded across arbitrarily many commits.
type HTTP struct {
	repoURL string
	client  *http.Client
	now     func() time.Time

	mu      sync.Mutex
	tip     oid
	objects map[oid]gitObject
}

// NewHTTP returns a smart HTTP client for the hub repository at rawURL. If
// rawURL names only the server root, DefaultRepo ("clearedstack.com.git") is
// used; a repository path without ".git" has ".git" appended.
func NewHTTP(rawURL string, timeout time.Duration) (*HTTP, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, ErrBadAddress
	}
	path := strings.TrimRight(u.Path, "/")
	if path == "" {
		path = "/" + DefaultRepo
	} else {
		for _, seg := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
			if seg == "" || seg == "." || seg == ".." {
				return nil, ErrBadAddress
			}
		}
		if !strings.HasSuffix(path, ".git") {
			path += ".git"
		}
	}
	u.Path = path
	u.RawPath = ""
	return &HTTP{
		repoURL: u.String(),
		now:     time.Now,
		client: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				// Cluster-private traffic: never route through environment proxies.
				Proxy:               nil,
				MaxIdleConns:        1,
				MaxIdleConnsPerHost: 1,
				MaxConnsPerHost:     1,
				IdleConnTimeout:     30 * time.Second,
				DisableCompression:  true,
				ForceAttemptHTTP2:   false,
				WriteBufferSize:     4 << 10,
				ReadBufferSize:      4 << 10,
			},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

// ReadFile reads path from refs/heads/main in the hub repository. When
// refs/heads/main does not exist yet or has no file at path, it returns
// ErrNotFound.
func (h *HTTP) ReadFile(ctx context.Context, path string) ([]byte, error) {
	parts, err := cleanRelPath(path)
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	remoteTip, caps, err := h.discover(ctx, serviceUpload)
	if err != nil {
		return nil, err
	}
	if err := h.syncTip(ctx, remoteTip, caps["multi_ack_detailed"], parts); err != nil {
		return nil, err
	}
	if h.tip.isZero() {
		return nil, ErrNotFound
	}
	entry, found, err := lookupPath(h.tip, h.objects, parts)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, ErrNotFound
	}
	if entry.mode == "40000" {
		return nil, ErrRefused
	}
	blob, ok := h.objects[entry.id]
	if !ok || blob.typ != objBlob {
		return nil, ErrCorrupt
	}
	return append([]byte{}, blob.data...), nil
}

// WriteFile commits data at path on refs/heads/main in the hub repository,
// preserving every other file and directory in the tree. If refs/heads/main
// already holds the exact bytes at path, WriteFile succeeds without making
// an empty commit.
func (h *HTTP) WriteFile(ctx context.Context, path string, data []byte, message string) error {
	parts, err := cleanRelPath(path)
	if err != nil {
		return err
	}
	if len(data) > maxObjectBytes {
		return ErrRefused
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	remoteTip, recvCaps, err := h.discover(ctx, serviceReceive)
	if err != nil {
		return err
	}
	if !recvCaps["report-status"] {
		return ErrRefused
	}
	if !remoteTip.isZero() && (remoteTip != h.tip || h.objects == nil) {
		upTip, upCaps, err := h.discover(ctx, serviceUpload)
		if err != nil {
			return err
		}
		remoteTip = upTip
		if err := h.syncTip(ctx, remoteTip, upCaps["multi_ack_detailed"], parts); err != nil {
			return err
		}
	}

	blobCopy := append([]byte{}, data...)
	blobID, err := hashObject(objBlob, blobCopy)
	if err != nil {
		return err
	}
	if !remoteTip.isZero() {
		existing, found, err := lookupPath(remoteTip, h.objects, parts)
		if err != nil {
			return err
		}
		if found && existing.mode == "100644" && existing.id == blobID {
			h.objects[blobID] = gitObject{typ: objBlob, data: blobCopy}
			return nil
		}
	}

	var rootTreeID oid
	if !remoteTip.isZero() {
		commitObj, ok := h.objects[remoteTip]
		if !ok || commitObj.typ != objCommit {
			return ErrCorrupt
		}
		rootTreeID, err = parseCommitTree(commitObj.data)
		if err != nil {
			return err
		}
	}

	staged := make(map[oid]gitObject, len(h.objects)+len(parts)+2)
	for k, v := range h.objects {
		staged[k] = v
	}
	var newObjects []gitObject
	seenNew := make(map[oid]bool)
	recordNew := func(id oid, obj gitObject) {
		staged[id] = obj
		if h.objects != nil {
			if _, already := h.objects[id]; already {
				return
			}
		}
		if !seenNew[id] {
			seenNew[id] = true
			newObjects = append(newObjects, obj)
		}
	}
	recordNew(blobID, gitObject{typ: objBlob, data: blobCopy})

	newRootID, err := spliceTree(rootTreeID, staged, parts, blobID, recordNew)
	if err != nil {
		return err
	}
	commitBytes := formatCommit(newRootID, remoteTip, h.now(), message)
	commitID, err := hashObject(objCommit, commitBytes)
	if err != nil {
		return err
	}
	recordNew(commitID, gitObject{typ: objCommit, data: commitBytes})

	packBytes, err := writePack(newObjects)
	if err != nil {
		return ErrCorrupt
	}
	var reqBody bytes.Buffer
	writePktString(&reqBody, remoteTip.String()+" "+commitID.String()+" "+MainRef+"\x00report-status\n")
	writePktFlush(&reqBody)
	reqBody.Write(packBytes)

	if err := h.push(ctx, reqBody.Bytes()); err != nil {
		return err
	}
	kept, err := retainTipState(commitID, staged, parts)
	if err != nil {
		return err
	}
	h.tip = commitID
	h.objects = kept
	return nil
}

func classifyStatus(status int) error {
	switch {
	case status == http.StatusTooManyRequests || status >= 500:
		return ErrUnavailable
	default:
		return ErrRefused
	}
}

func (h *HTTP) discover(ctx context.Context, service string) (oid, map[string]bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.repoURL+"/info/refs?service="+service, nil)
	if err != nil {
		return zeroOID, nil, ErrUnavailable
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return zeroOID, nil, ErrUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return zeroOID, nil, classifyStatus(resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "application/x-"+service+"-advertisement") {
		return zeroOID, nil, ErrCorrupt
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxAdvBytes+1))
	if err != nil {
		return zeroOID, nil, ErrUnavailable
	}
	if len(data) > maxAdvBytes {
		return zeroOID, nil, ErrCorrupt
	}

	r := bytes.NewReader(data)
	banner, flush, err := readPktLine(r)
	if err != nil || flush || strings.TrimSuffix(string(banner), "\n") != "# service="+service {
		return zeroOID, nil, ErrCorrupt
	}
	if _, flush, err := readPktLine(r); err != nil || !flush {
		return zeroOID, nil, ErrCorrupt
	}

	caps := make(map[string]bool)
	var mainTip oid
	for first := true; ; first = false {
		line, flush, err := readPktLine(r)
		if err != nil {
			return zeroOID, nil, ErrCorrupt
		}
		if flush {
			break
		}
		text := strings.TrimSuffix(string(line), "\n")
		if first {
			var capStr string
			text, capStr, _ = strings.Cut(text, "\x00")
			for _, c := range strings.Fields(capStr) {
				caps[c] = true
			}
		}
		hexID, refName, ok := strings.Cut(text, " ")
		if !ok {
			return zeroOID, nil, ErrCorrupt
		}
		id, err := parseOID(hexID)
		if err != nil {
			return zeroOID, nil, err
		}
		if refName == MainRef {
			mainTip = id
		}
	}
	return mainTip, caps, nil
}

func (h *HTTP) syncTip(ctx context.Context, remoteTip oid, multiAck bool, parts []string) error {
	if remoteTip.isZero() {
		h.tip = zeroOID
		h.objects = nil
		return nil
	}
	if remoteTip == h.tip && h.objects != nil {
		entry, found, err := lookupPath(h.tip, h.objects, parts)
		if err == nil && (!found || entry.mode == "40000" || h.objects[entry.id].typ == objBlob) {
			return nil
		}
	}

	var reqBody bytes.Buffer
	wantLine := "want " + remoteTip.String()
	if multiAck {
		wantLine += " multi_ack_detailed"
	}
	writePktString(&reqBody, wantLine+"\n")
	writePktFlush(&reqBody)
	writePktString(&reqBody, "done\n")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.repoURL+"/"+serviceUpload, &reqBody)
	if err != nil {
		return ErrUnavailable
	}
	req.Header.Set("Content-Type", "application/x-"+serviceUpload+"-request")
	req.Header.Set("Accept", "application/x-"+serviceUpload+"-result")

	resp, err := h.client.Do(req)
	if err != nil {
		return ErrUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return classifyStatus(resp.StatusCode)
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/x-"+serviceUpload+"-result") {
		return ErrCorrupt
	}

	limited := io.LimitReader(resp.Body, maxResponseCap)
	br := bufio.NewReaderSize(limited, 32<<10)
	for {
		line, flush, err := readPktLine(br)
		if err != nil || flush {
			return ErrCorrupt
		}
		fields := strings.Fields(strings.TrimSuffix(string(line), "\n"))
		if len(fields) == 0 {
			return ErrCorrupt
		}
		if fields[0] == "NAK" || (fields[0] == "ACK" && len(fields) == 2) {
			break
		}
		if fields[0] != "ACK" {
			return ErrCorrupt
		}
	}

	fetched, err := unpackPack(br, h.objects)
	if err != nil {
		return err
	}
	kept, err := retainTipState(remoteTip, fetched, parts)
	if err != nil {
		return err
	}
	h.tip = remoteTip
	h.objects = kept
	return nil
}

func (h *HTTP) push(ctx context.Context, payload []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.repoURL+"/"+serviceReceive, bytes.NewReader(payload))
	if err != nil {
		return ErrUnavailable
	}
	req.Header.Set("Content-Type", "application/x-"+serviceReceive+"-request")
	req.Header.Set("Accept", "application/x-"+serviceReceive+"-result")

	resp, err := h.client.Do(req)
	if err != nil {
		return ErrUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return classifyStatus(resp.StatusCode)
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/x-"+serviceReceive+"-result") {
		return ErrCorrupt
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxStatusBytes+1))
	if err != nil {
		return ErrUnavailable
	}
	if len(data) > maxStatusBytes {
		return ErrCorrupt
	}

	r := bytes.NewReader(data)
	unpackOK := false
	refOK := false
	for {
		line, flush, err := readPktLine(r)
		if err != nil {
			return ErrCorrupt
		}
		if flush {
			break
		}
		text := strings.TrimSuffix(string(line), "\n")
		switch {
		case text == "unpack ok":
			unpackOK = true
		case strings.HasPrefix(text, "unpack "):
			return ErrRefused
		case text == "ok "+MainRef:
			refOK = true
		case strings.HasPrefix(text, "ng "+MainRef+" "):
			reason := strings.ToLower(strings.TrimPrefix(text, "ng "+MainRef+" "))
			if strings.Contains(reason, "changed since") ||
				strings.Contains(reason, "fast-forward") ||
				strings.Contains(reason, "lock") ||
				strings.Contains(reason, "stale") {
				return ErrUnavailable
			}
			return ErrRefused
		}
	}
	if !unpackOK || !refOK {
		return ErrCorrupt
	}
	return nil
}

func lookupPath(commitID oid, objects map[oid]gitObject, parts []string) (treeEntry, bool, error) {
	commitObj, ok := objects[commitID]
	if !ok || commitObj.typ != objCommit {
		return treeEntry{}, false, ErrCorrupt
	}
	treeID, err := parseCommitTree(commitObj.data)
	if err != nil {
		return treeEntry{}, false, err
	}
	cur := treeID
	for i, name := range parts {
		treeObj, ok := objects[cur]
		if !ok || treeObj.typ != objTree {
			return treeEntry{}, false, ErrCorrupt
		}
		entries, err := parseTree(treeObj.data)
		if err != nil {
			return treeEntry{}, false, err
		}
		var matched *treeEntry
		for j := range entries {
			if entries[j].name == name {
				matched = &entries[j]
				break
			}
		}
		if matched == nil {
			return treeEntry{}, false, nil
		}
		if i == len(parts)-1 {
			return *matched, true, nil
		}
		if matched.mode != "40000" {
			return treeEntry{}, false, ErrRefused
		}
		cur = matched.id
	}
	return treeEntry{}, false, nil
}

func spliceTree(treeID oid, objects map[oid]gitObject, parts []string, blobID oid, record func(oid, gitObject)) (oid, error) {
	var entries []treeEntry
	if !treeID.isZero() {
		treeObj, ok := objects[treeID]
		if !ok || treeObj.typ != objTree {
			return zeroOID, ErrCorrupt
		}
		var err error
		entries, err = parseTree(treeObj.data)
		if err != nil {
			return zeroOID, err
		}
	}

	name := parts[0]
	var childID oid
	var childMode string
	idx := -1
	for i, e := range entries {
		if e.name == name {
			idx = i
			break
		}
	}

	if len(parts) == 1 {
		if idx >= 0 && entries[idx].mode == "40000" {
			return zeroOID, ErrRefused
		}
		childMode = "100644"
		childID = blobID
	} else {
		var subTreeID oid
		if idx >= 0 {
			if entries[idx].mode != "40000" {
				return zeroOID, ErrRefused
			}
			subTreeID = entries[idx].id
		}
		var err error
		childID, err = spliceTree(subTreeID, objects, parts[1:], blobID, record)
		if err != nil {
			return zeroOID, err
		}
		childMode = "40000"
	}

	updated := treeEntry{mode: childMode, name: name, id: childID}
	if idx >= 0 {
		entries[idx] = updated
	} else {
		entries = append(entries, updated)
	}
	raw := formatTree(entries)
	newTreeID, err := hashObject(objTree, raw)
	if err != nil {
		return zeroOID, err
	}
	record(newTreeID, gitObject{typ: objTree, data: raw})
	return newTreeID, nil
}

func retainTipState(commitID oid, pool map[oid]gitObject, parts []string) (map[oid]gitObject, error) {
	commitObj, ok := pool[commitID]
	if !ok || commitObj.typ != objCommit {
		return nil, ErrCorrupt
	}
	rootID, err := parseCommitTree(commitObj.data)
	if err != nil {
		return nil, err
	}
	kept := map[oid]gitObject{commitID: commitObj}
	var walkTrees func(id oid, depth int) error
	walkTrees = func(id oid, depth int) error {
		if depth > maxTreeDepth {
			return ErrCorrupt
		}
		if _, seen := kept[id]; seen {
			return nil
		}
		tObj, ok := pool[id]
		if !ok || tObj.typ != objTree {
			return ErrCorrupt
		}
		entries, err := parseTree(tObj.data)
		if err != nil {
			return err
		}
		kept[id] = tObj
		for _, e := range entries {
			if e.mode == "40000" {
				if err := walkTrees(e.id, depth+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walkTrees(rootID, 0); err != nil {
		return nil, err
	}
	entry, found, err := lookupPath(commitID, pool, parts)
	if err != nil {
		return nil, err
	}
	if found && entry.mode != "40000" {
		blobObj, ok := pool[entry.id]
		if !ok || blobObj.typ != objBlob {
			return nil, ErrCorrupt
		}
		kept[entry.id] = blobObj
	}
	return kept, nil
}
