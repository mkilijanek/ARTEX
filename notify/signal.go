package notify

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// Operator settings of the Signal channel. Both live in the server environment
// and are never part of a channel config: the config holds recipients only, so
// the UI cannot point Signal at another endpoint or another sender account.
const (
	// SignalAPIEnv is the bridge endpoint, `unix:/absolute/path`.
	SignalAPIEnv = "ARTEX_SIGNAL_API"
	// SignalNumberEnv is the sender account (E.164) registered on the bridge.
	SignalNumberEnv = "ARTEX_SIGNAL_NUMBER"
)

const (
	// signalTextLimit is the message size limit in **bytes**. Conservative
	// against Signal's long-text threshold (exact value unverified, plan AC16).
	signalTextLimit = 2000
	// signalReservedBytes is reserved for the digest title and the footer link
	// when packing items (the footer itself is measured separately).
	signalReservedBytes = 200
	// signalSendURL: the host is a fixed placeholder, the client dials the
	// configured socket whatever the URL says.
	signalSendURL = "http://signal-bridge/v2/send"
	// signalSendTimeout stays below the notifier's 30 s send timeout.
	signalSendTimeout = 25 * time.Second

	signalUnreachable = "Signal bridge socket unreachable (is docker-compose.signal.yml up?)"
)

var (
	signalNumberRe = regexp.MustCompile(`^\+[1-9][0-9]{6,14}$`)
	// Masking patterns of signalScrub (step 2).
	signalNumberMaskRe = regexp.MustCompile(`\+?[1-9][0-9]{6,14}`)
	signalGroupMaskRe  = regexp.MustCompile(`group\.[A-Za-z0-9+/=_-]+`)
	// signalStatusRe picks the HTTP status out of the text doJSONVia builds.
	signalStatusRe = regexp.MustCompile(`\(HTTP ([0-9]{3})\)`)
)

// signalSettings is the operator configuration, parsed once at start.
type signalSettings struct {
	socket string // "" when ARTEX_SIGNAL_API is unset or invalid
	number string // "" when ARTEX_SIGNAL_NUMBER is unset or invalid
	// apiErr / numberErr hold the text shown to the configurator when the
	// matching value is unusable. They name the variable and never the value.
	apiErr    string
	numberErr string
	client    *http.Client // dials socket only; nil when socket == ""
	// logLines are the `[notify] signal: ...` lines the parse produced. init cannot log them: the
	// process log sink (server.StartLogCapture) does not exist yet, so LogSignalConfig writes them.
	logLines []string
}

var signalState atomic.Pointer[signalSettings]

func init() { LoadSignalEnv(os.Getenv) }

// LoadSignalEnv parses ARTEX_SIGNAL_API and ARTEX_SIGNAL_NUMBER with getenv and
// makes the result the process-wide Signal configuration. It runs once at start
// (init, with os.Getenv); tests call it with a fake getenv. An invalid value
// leaves Signal "not configured" and stores one `[notify] signal: ...` line
// naming the variable, with the number masked to its last 4 digits; the process
// keeps running. It does not log: LogSignalConfig writes the stored lines once
// the log sink is up.
func LoadSignalEnv(getenv func(string) string) {
	st := &signalSettings{}

	api := strings.TrimSpace(getenv(SignalAPIEnv))
	switch {
	case api == "":
		st.apiErr = "Signal bridge is not configured: set " + SignalAPIEnv + " (unix:/absolute/path)"
	case !strings.HasPrefix(api, "unix:") || !filepath.IsAbs(strings.TrimPrefix(api, "unix:")) ||
		strings.ContainsRune(api, 0):
		st.apiErr = "Signal bridge is not configured: " + SignalAPIEnv + " must be unix:/absolute/path"
		st.logLines = append(st.logLines, fmt.Sprintf("[notify] signal: %s is invalid, expected unix:/absolute/path; Signal channels stay disabled", SignalAPIEnv))
	default:
		st.socket = strings.TrimPrefix(api, "unix:")
		st.client = newSignalClient(st.socket)
	}

	num := strings.TrimSpace(getenv(SignalNumberEnv))
	switch {
	case num == "":
		st.numberErr = "Signal sender is not configured: set " + SignalNumberEnv
	case !signalNumberRe.MatchString(num):
		st.numberErr = "Signal sender is not configured: " + SignalNumberEnv + " must be an E.164 number (+ and 7 to 15 digits)"
		st.logLines = append(st.logLines, fmt.Sprintf("[notify] signal: %s is invalid (got %s), expected E.164 (+ and 7 to 15 digits); Signal channels stay disabled",
			SignalNumberEnv, signalMaskTail(num)))
	default:
		st.number = num
	}

	if old := signalState.Swap(st); old != nil && old.client != nil {
		old.client.CloseIdleConnections()
	}
}

// LogSignalConfig writes the lines LoadSignalEnv stored for an invalid setting. main calls it once,
// right after the log sink is installed, so the lines reach the captured log and not just stderr.
func LogSignalConfig() {
	for _, l := range signalState.Load().logLines {
		log.Print(l)
	}
}

// SignalSocketPath returns the bridge socket path, or "" when ARTEX_SIGNAL_API
// is unset or invalid.
func SignalSocketPath() string { return signalState.Load().socket }

// signalRecheck is the hook main installs to re-run the isolation self-check C12 (the bridge socket
// is not reachable by tools). It is a hook, not an import of the sandbox package, so notify stays
// independent of it. It returns nil only when the check ran and its result was published.
// signalSentOnce makes the send path call it after the first success, and again after the next
// success when the call returned an error.
var (
	signalRecheck  atomic.Pointer[func(context.Context) error]
	signalSentOnce atomic.Bool
)

// signalRecheckTimeout bounds the background re-run started by a send.
const signalRecheckTimeout = 2 * time.Minute

// SetSignalRecheck installs the C12 re-run hook; nil removes it.
func SetSignalRecheck(f func(context.Context) error) {
	if f == nil {
		signalRecheck.Store(nil)
		return
	}
	signalRecheck.Store(&f)
}

// RecheckSignal calls the C12 re-run hook when one is installed, and waits for it. It returns nil
// without a hook, and otherwise what the hook returns: an error means the check did not run.
func RecheckSignal(ctx context.Context) error {
	if f := signalRecheck.Load(); f != nil {
		return (*f)(ctx)
	}
	return nil
}

// signalStatusKey carries a *int in the request context; signalTransport stores the response
// status there, because doJSONVia returns the body of a 2xx response and not its status code.
type signalStatusKey struct{}

// signalTransport is the bridge transport plus the status recording described at signalStatusKey.
type signalTransport struct{ base *http.Transport }

func (t *signalTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if resp != nil {
		if p, ok := req.Context().Value(signalStatusKey{}).(*int); ok {
			*p = resp.StatusCode
		}
	}
	return resp, err
}

func (t *signalTransport) CloseIdleConnections() { t.base.CloseIdleConnections() }

// newSignalClient builds the bridge client. It differs from the shared
// httpClient on purpose:
//   - the Transport dials the socket path and nothing else, whatever host the
//     URL names, so no config value can steer the endpoint;
//   - Proxy is nil, so HTTP(S)_PROXY from the environment cannot hijack the
//     request;
//   - redirects are never followed: the 3xx response itself comes back and
//     doJSONVia classifies it as permanent.
func newSignalClient(socket string) *http.Client {
	return &http.Client{
		Timeout: signalSendTimeout,
		Transport: &signalTransport{base: &http.Transport{
			Proxy: nil,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				conn, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "unix", socket)
				if err != nil {
					// The raw error carries "connect: no such file or directory" and
					// the socket path; say what the operator can act on instead.
					if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED) {
						return nil, errors.New(signalUnreachable)
					}
					return nil, err
				}
				return conn, nil
			},
			MaxIdleConns:    2,
			IdleConnTimeout: 30 * time.Second,
		}},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// signalMaskTail renders a value as "…" plus its last 4 characters.
func signalMaskTail(s string) string {
	r := []rune(s)
	if len(r) <= 4 {
		return "…"
	}
	return "…" + string(r[len(r)-4:])
}

// signalMaskGroup renders a group id as group.…<last4> (padding ignored).
func signalMaskGroup(id string) string {
	return "group." + signalMaskTail(strings.TrimRight(id, "="))
}

// signalChannel implements the Signal channel through a signal-cli-rest-api
// bridge reachable over a Unix socket (see the plan, design D1-D6).
//
// Platform traits:
//   - There is no credential in the channel config; the sender account lives on
//     the bridge and in ARTEX_SIGNAL_NUMBER. Recipients are PII, not secrets.
//   - The bridge answers 400 {"error": ...} for every send failure except rate
//     limiting (429), and 201 with per-recipient errors on partial delivery.
//   - Plain text only (text_mode "normal"): finding text comes from scanned
//     targets, and styled mode would let it inject formatting.
type signalChannel struct{}

func (signalChannel) Kind() string { return KindSignal }

// Signal publishes no limit; same conservative value as Telegram.
func (signalChannel) DefaultRatePerMin() int { return 20 }

// SecretKeys: nothing in the config is a credential. Non-nil so the meta API
// renders [] and not null.
func (signalChannel) SecretKeys() []string { return []string{} }

// DestinationKeys: the endpoint is operator config, not channel config.
func (signalChannel) DestinationKeys() []string { return []string{} }

const (
	signalTypeGroup   = "group"
	signalTypeNumbers = "numbers"

	signalKindNumber      = "number"
	signalKindGroup       = "group"
	signalKindUnsupported = "unsupported"
)

// signalRecipientKind classifies one recipient string. It is the single
// extension point for new recipient types (usernames, see plan D6).
func signalRecipientKind(s string) string {
	switch {
	case signalNumberRe.MatchString(s):
		return signalKindNumber
	case strings.HasPrefix(s, "group."):
		return signalKindGroup
	default:
		return signalKindUnsupported
	}
}

// signalGroupInner checks the bridge's own group-id rule: base64 (std) that
// decodes, and whose result decodes again, to exactly 32 bytes. It returns the
// once-decoded id, which is how signal-cli knows the group and how the bridge
// may echo it back.
func signalGroupInner(id string) (string, bool) {
	once, err := base64.StdEncoding.DecodeString(id)
	if err != nil {
		return "", false
	}
	twice, err := base64.StdEncoding.DecodeString(string(once))
	if err != nil || len(twice) != 32 {
		return "", false
	}
	return string(once), true
}

// signalRecipients reads the recipient list: trimmed, empty entries dropped,
// duplicates removed (first occurrence wins). A non-string entry is an error
// instead of being dropped silently.
func signalRecipients(cfg map[string]any) ([]string, error) {
	if raw, ok := cfg["recipients"].([]any); ok {
		for i, v := range raw {
			if _, ok := v.(string); !ok {
				return nil, fmt.Errorf("recipients[%d]: must be a string", i)
			}
		}
	}
	seen := map[string]bool{}
	var out []string
	for _, r := range cfgStrings(cfg, "recipients") {
		if !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	return out, nil
}

// Validate checks recipients and recipient_type only, then the operator
// settings. Messages name the field and never echo a recipient value, because
// Send passes them on as a delivery error.
func (signalChannel) Validate(cfg map[string]any) error {
	if err := signalValidateRecipients(cfg); err != nil {
		return err
	}
	st := signalState.Load()
	if st.numberErr != "" {
		return errors.New(st.numberErr)
	}
	if st.apiErr != "" {
		return errors.New(st.apiErr)
	}
	return nil
}

func signalValidateRecipients(cfg map[string]any) error {
	rtype := cfgString(cfg, "recipient_type")
	if rtype != "" && rtype != signalTypeGroup && rtype != signalTypeNumbers {
		return errors.New(`recipient_type: must be "group" or "numbers"`)
	}
	recipients, err := signalRecipients(cfg)
	if err != nil {
		return err
	}
	if len(recipients) == 0 {
		return errors.New("recipients: at least one recipient is required")
	}
	var numbers, groups int
	for i, r := range recipients {
		switch signalRecipientKind(r) {
		case signalKindNumber:
			numbers++
		case signalKindGroup:
			if _, ok := signalGroupInner(strings.TrimPrefix(r, "group.")); !ok {
				return fmt.Errorf("recipients[%d]: not a valid group id (group.<id>, base64 of a base64-encoded 32-byte id)", i)
			}
			groups++
		default:
			return fmt.Errorf("recipients[%d]: must be a phone number like +15555550100 or group.<id>; usernames and UUIDs are not supported", i)
		}
	}
	switch {
	case groups > 0 && numbers > 0:
		return errors.New("recipients: phone numbers and a group cannot be mixed; use one channel for each")
	case groups > 1:
		return errors.New("recipients: only one group per channel; create one channel for each group")
	case rtype == signalTypeGroup && groups != 1:
		return errors.New(`recipient_type: "group" needs exactly one group.<id> in recipients`)
	case rtype == signalTypeNumbers && groups > 0:
		return errors.New(`recipient_type: "numbers" does not accept a group in recipients`)
	}
	return nil
}

// signalPayload is the exact request body of POST /v2/send; nothing else is sent.
type signalPayload struct {
	Number     string   `json:"number"`
	Recipients []string `json:"recipients"`
	Message    string   `json:"message"`
	TextMode   string   `json:"text_mode"`
}

func (c signalChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	st := signalState.Load()
	recipients, _ := signalRecipients(cfg)
	scrub := newSignalScrubber(st.number, recipients)

	if err := c.Validate(cfg); err != nil {
		return 0, scrub.err(Permanent(err))
	}
	if len(m.Items) == 0 {
		return 0, Permanent(errors.New("nothing to send: the message has no items"))
	}
	text, kept := signalText(m)
	payload := signalPayload{Number: st.number, Recipients: recipients, Message: text, TextMode: "normal"}

	var status int
	raw, err := doJSONVia(context.WithValue(ctx, signalStatusKey{}, &status), st.client, scrub.body, "POST", signalSendURL, nil, payload)
	if err != nil {
		return 0, scrub.err(signalClassify(err))
	}
	if err := signalCheckDelivery(status, raw, recipients, scrub); err != nil {
		return 0, scrub.err(err)
	}
	// The first delivery proves the socket exists now: let the isolation check C12 look again
	// (in the background, so the delivery is not held up by the tool shell). The re-run outlives the
	// request but is bounded; when it did not run, the next successful send tries again.
	if signalSentOnce.CompareAndSwap(false, true) {
		go func() {
			rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), signalRecheckTimeout)
			defer cancel()
			if err := RecheckSignal(rctx); err != nil {
				log.Printf("[notify] signal: isolation recheck skipped: %v", err)
				signalSentOnce.Store(false)
			}
		}()
	}
	return kept, nil
}

// signalClassify turns a doJSONVia error into the Signal-specific text. 429 is
// retryable and must not carry the bridge body (it holds challenge_tokens).
func signalClassify(err error) error {
	if mm := signalStatusRe.FindStringSubmatch(err.Error()); mm != nil && mm[1] == "429" {
		return errors.New("Signal rate limit: solve the rate-limit challenge on the bridge (see the deployment README); delivery is retried")
	}
	return err
}

// signalCheckDelivery inspects the 2xx body. Only two shapes count as accepted by the bridge: an
// object with a non-empty timestamp, or an array whose every element is such an object; anything
// else (an empty body, {}, [], null, an error object, a missing or zero timestamp) is a permanent
// "unexpected success response", because a retry would not tell whether the message went out. The
// bridge also reports recipients it could not reach inside a successful response; any such
// recipient makes the delivery a permanent failure (a retry would send the message again to the
// recipients that already got it).
func signalCheckDelivery(status int, raw []byte, recipients []string, scrub *signalScrubber) error {
	type result struct {
		Timestamp any `json:"timestamp"`
		Error     any `json:"error"`
		Errors    struct {
			Recipients []struct {
				Number   string `json:"number"`
				UUID     string `json:"uuid"`
				Username string `json:"username"`
				Reason   string `json:"reason"`
			} `json:"recipients"`
		} `json:"errors"`
	}
	// unexpected never quotes the body; the only hint is the text of an {"error": ...} member,
	// scrubbed and cut short.
	unexpected := func(hint string) error {
		msg := fmt.Sprintf("Signal bridge returned an unexpected success response (HTTP %d)", status)
		if hint = OneLine(scrub.text(hint), 80); hint != "" {
			msg += ": " + hint
		}
		return Permanent(errors.New(msg))
	}
	// accepted reports whether one element has the shape of an accepted send.
	accepted := func(r *result) bool {
		if r == nil || r.Error != nil {
			return false
		}
		switch ts := r.Timestamp.(type) {
		case string:
			return strings.TrimSpace(ts) != ""
		case float64:
			return ts != 0
		}
		return false
	}
	errHint := func(r *result) string {
		if r != nil {
			if e, ok := r.Error.(string); ok {
				return e
			}
		}
		return ""
	}
	var results []*result
	trimmed := strings.TrimSpace(string(raw))
	switch {
	case trimmed == "":
		return unexpected("")
	case strings.HasPrefix(trimmed, "["):
		if err := json.Unmarshal(raw, &results); err != nil {
			return Permanent(fmt.Errorf("Signal bridge returned an unreadable success response: %s", scrub.body(raw)))
		}
		if len(results) == 0 {
			return unexpected("")
		}
	default:
		var one *result
		if err := json.Unmarshal(raw, &one); err != nil {
			return Permanent(fmt.Errorf("Signal bridge returned an unreadable success response: %s", scrub.body(raw)))
		}
		results = []*result{one}
	}
	for _, r := range results {
		if !accepted(r) {
			return unexpected(errHint(r))
		}
	}
	var failed []string
	for _, r := range results {
		for _, f := range r.Errors.Recipients {
			id := f.Number
			if id == "" {
				id = f.UUID
			}
			if id == "" {
				id = f.Username
			}
			reason := OneLine(scrub.text(f.Reason), 80)
			if reason == "" {
				reason = "no reason given"
			}
			failed = append(failed, fmt.Sprintf("%s (%s)", signalMaskTail(id), reason))
		}
	}
	if len(failed) == 0 {
		return nil
	}
	if len(recipients) == 1 && strings.HasPrefix(recipients[0], "group.") {
		return Permanent(fmt.Errorf("delivered to the group, but failed for %d member(s): %s", len(failed), strings.Join(failed, ", ")))
	}
	delivered := len(recipients) - len(failed)
	if delivered < 0 {
		delivered = 0
	}
	return Permanent(fmt.Errorf("delivered to %d of %d; failed: %s", delivered, len(recipients), strings.Join(failed, ", ")))
}

// signalScrubber removes phone numbers and group ids from text that leaves the
// channel (errors, and through them last_error, the delivery history, the
// test-send 502 and log lines).
type signalScrubber struct {
	pairs []string // strings.NewReplacer arguments: literal, replacement, ...
	repl  *strings.Replacer
}

// newSignalScrubber builds the literal list: the global sender, each recipient
// (with and without the leading +), and for a group both the group.<id> form,
// the bare id and the once-decoded inner id. Only well-formed values are added;
// a replace of an arbitrary short string would mangle the text.
func newSignalScrubber(sender string, recipients []string) *signalScrubber {
	s := &signalScrubber{}
	add := func(lit, mask string) {
		if lit != "" {
			s.pairs = append(s.pairs, lit, mask)
		}
	}
	addNumber := func(n string) {
		if signalNumberRe.MatchString(n) {
			add(n, signalMaskTail(n))
			add(strings.TrimPrefix(n, "+"), signalMaskTail(n))
		}
	}
	addNumber(sender)
	for _, r := range recipients {
		switch signalRecipientKind(r) {
		case signalKindNumber:
			addNumber(r)
		case signalKindGroup:
			id := strings.TrimPrefix(r, "group.")
			inner, ok := signalGroupInner(id)
			if !ok {
				continue
			}
			add(r, signalMaskGroup(id))
			add(id, signalMaskGroup(id))
			add(inner, signalMaskGroup(id))
		}
	}
	// NewReplacer tries the arguments in order at each position, so the longer
	// literal of a pair has to come first; the number loop above already adds
	// the "+" form before the bare one, and group.<id> before <id>.
	s.repl = strings.NewReplacer(s.pairs...)
	return s
}

// text runs steps 1 and 2: literals first, then the patterns for values the
// literals did not know. It does not truncate; callers decide where the cut
// goes (after masking, never before).
func (s *signalScrubber) text(in string) string {
	out := s.repl.Replace(in)
	out = signalGroupMaskRe.ReplaceAllStringFunc(out, func(g string) string {
		return signalMaskGroup(strings.TrimPrefix(g, "group."))
	})
	return signalNumberMaskRe.ReplaceAllStringFunc(out, signalMaskTail)
}

// body is the scrub function handed to doJSONVia: it receives the full capped
// response body and returns the error fragment. The body is read as
// {"error": ...} when it parses, so fields such as challenge_tokens and account
// are dropped instead of quoted; anything else is quoted raw. Masking happens
// first and OneLine(…, 200) last, so a cut can never leave the head of a number.
func (s *signalScrubber) body(raw []byte) string {
	text := string(raw)
	var obj map[string]any
	if json.Unmarshal(raw, &obj) == nil {
		if e, ok := obj["error"].(string); ok {
			text = e
		} else {
			text = "(no error text)"
		}
	}
	return OneLine(s.text(text), 200)
}

// err masks the whole error text, keeping the permanent marker. Every error the
// channel returns passes through here.
func (s *signalScrubber) err(err error) error {
	if err == nil {
		return nil
	}
	clean := errors.New(s.text(err.Error()))
	if IsPermanent(err) {
		return Permanent(clean)
	}
	return clean
}

// signalText renders the message as plain text and returns it with the number
// of items written (see Channel.Send). Plain text needs no escaping; titles come
// from the shared helpers, which return the unescaped original.
func signalText(m Message) (string, int) {
	if !m.Batch {
		it := m.Items[0]
		var b strings.Builder
		b.WriteString(markdownTitle(m))
		if it.IsStatusChange() {
			b.WriteString(fmt.Sprintf("\nStatus change: %s → %s", StatusLabel(it.FromStatus), StatusLabel(it.ToStatus)))
		}
		if it.VulnClass != "" && it.VulnClass != it.Title() {
			b.WriteString("\nType: " + OneLine(it.VulnClass, 0))
		}
		if a := assetLine(it.Assets, maxAssetsShown); a != "" {
			b.WriteString("\nAssets: " + OneLine(a, 0))
		}
		if s := OneLine(it.Summary, maxSummaryRunes); s != "" {
			b.WriteString("\nSummary: " + s)
		}
		if it.DetailURL != "" {
			b.WriteString("\nView details: " + it.DetailURL)
		}
		// A single oversized item is sent truncated: partial information about
		// one finding beats a delivery that can never succeed.
		return TruncateBytes(b.String(), signalTextLimit), 1
	}

	footer := ""
	if m.HomeURL != "" {
		footer = "\n\nView all on the platform: " + m.HomeURL
	}
	line := func(it Item, idx int) string {
		// One line per item: a title with a newline must not forge a new entry.
		return "\n" + OneLine(telegramBatchLine(it, idx+1), 0)
	}
	kept := packItemCount(m.Items, signalTextLimit, signalReservedBytes, footer, byteSize, line)
	items := m.Items[:kept]
	var b strings.Builder
	b.WriteString(OneLine(telegramBatchTitle(m, items, len(m.Items)), 0))
	for i, it := range items {
		b.WriteString(line(it, i))
	}
	b.WriteString(footer)
	return TruncateBytes(b.String(), signalTextLimit), kept
}
