package intelligence

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	cdpruntime "github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

var errPage = errors.New("browser operation could not be completed safely")

type pageDriver struct {
	ctx      context.Context
	cancel   context.CancelFunc
	hosts    map[string]bool
	queue    chan *fetch.EventRequestPaused
	done     chan struct{}
	closed   atomic.Bool
	failed   atomic.Bool
	blocked  atomic.Bool
	destroy  sync.Once
	close    sync.Once
	closeErr error
	world    cdpruntime.ExecutionContextID
	frame    cdp.FrameID
	targets  map[int]string
	nextID   int
}

func newPage(ctx context.Context, startingURL string, allowedHosts []string) (*pageDriver, error) {
	u, err := pageURL(startingURL)
	if err != nil || ctx.Err() != nil {
		return nil, errPage
	}
	connection := chromedp.FromContext(ctx)
	if connection == nil || connection.Target == nil || connection.Browser == nil {
		return nil, errPage
	}
	hosts := make(map[string]bool)
	if len(allowedHosts) == 0 {
		allowedHosts = []string{"*"}
	}
	for _, host := range allowedHosts {
		if host == "*" {
			if len(allowedHosts) != 1 {
				return nil, errPage
			}
			hosts[host] = true
			continue
		}
		if host == "" || strings.TrimSpace(host) != host || strings.ContainsAny(host, "/?#@*\\") {
			return nil, errPage
		}
		if net.ParseIP(host) == nil && strings.Contains(host, ":") {
			return nil, errPage
		}
		hosts[strings.ToLower(host)] = true
	}
	if !hosts["*"] && !hosts[strings.ToLower(u.Hostname())] {
		return nil, errPage
	}
	guard, cancel := context.WithCancel(context.WithoutCancel(ctx))
	p := &pageDriver{ctx: guard, cancel: cancel, hosts: hosts, queue: make(chan *fetch.EventRequestPaused, 128), done: make(chan struct{}), nextID: 1}
	var workers sync.WaitGroup
	for range 4 {
		workers.Go(p.requests)
	}
	go func() { workers.Wait(); close(p.done) }()
	chromedp.ListenTarget(guard, func(event any) {
		request, ok := event.(*fetch.EventRequestPaused)
		if !ok {
			return
		}
		select {
		case p.queue <- request:
		default:
			p.failed.Store(true)
			p.destroyTarget()
		}
	})
	bounded, stop := context.WithTimeout(ctx, 8*time.Second)
	defer stop()
	// Pinned Lightpanda stores Fetch interception on the browser context and
	// routes popup-document events to this enabling session as well.
	if err := chromedp.Run(bounded, fetch.Enable()); err != nil {
		_ = p.Close()
		return nil, errPage
	}
	return p, nil
}

func pageURL(raw string) (*url.URL, error) {
	if len(raw) == 0 || len(raw) > 8192 {
		return nil, errPage
	}
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Hostname() == "" || u.Scheme != "http" && u.Scheme != "https" {
		return nil, errPage
	}
	return u, nil
}

func safePageURL(raw string) string {
	u, err := pageURL(raw)
	if err != nil {
		return ""
	}
	u.RawQuery, u.Fragment, u.RawFragment, u.ForceQuery = "", "", "", false
	return u.String()
}

func (p *pageDriver) allowed(raw string) bool {
	u, err := pageURL(raw)
	return err == nil && (p.hosts["*"] || p.hosts[strings.ToLower(u.Hostname())])
}

func (p *pageDriver) requests() {
	for {
		select {
		case <-p.ctx.Done():
			return
		case request := <-p.queue:
			ctx, cancel := context.WithTimeout(p.ctx, 2*time.Second)
			ctx = cdp.WithExecutor(ctx, chromedp.FromContext(p.ctx).Target)
			u, err := pageURL(request.Request.URL)
			permitted := !p.closed.Load() && err == nil && (p.hosts["*"] || p.hosts[strings.ToLower(u.Hostname())] || request.ResourceType != network.ResourceTypeDocument && (request.Request.Method == "GET" || request.Request.Method == "HEAD"))
			if permitted {
				err = fetch.ContinueRequest(request.RequestID).Do(ctx)
			} else {
				if request.ResourceType == network.ResourceTypeDocument {
					p.blocked.Store(true)
				}
				err = fetch.FailRequest(request.RequestID, network.ErrorReasonBlockedByClient).Do(ctx)
			}
			cancel()
			if err != nil && !p.closed.Load() {
				p.failed.Store(true)
				p.destroyTarget()
			}
		}
	}
}

func (p *pageDriver) destroyTarget() {
	p.destroy.Do(func() {
		go func() {
			ctx, cancel := context.WithTimeout(context.WithoutCancel(p.ctx), 2*time.Second)
			defer cancel()
			connection := chromedp.FromContext(p.ctx)
			_ = target.CloseTarget(connection.Target.TargetID).Do(cdp.WithExecutor(ctx, connection.Browser))
		}()
	})
}

func (p *pageDriver) Close() error {
	p.close.Do(func() {
		p.closed.Store(true)
		ctx, cancel := context.WithTimeout(context.WithoutCancel(p.ctx), 3*time.Second)
		defer cancel()
		connection := chromedp.FromContext(p.ctx)
		cleanup := cdp.WithExecutor(ctx, connection.Target)
		// Stop the action's pending navigation before disabling interception: Fetch.disable
		// resumes requests that the browser still has paused.
		if err := page.StopLoading().Do(cleanup); err != nil {
			p.closeErr = errPage
			p.destroyTarget()
		} else if err := fetch.Disable().Do(cleanup); err != nil {
			p.closeErr = errPage
			p.destroyTarget()
		}
		p.cancel()
		select {
		case <-p.done:
		case <-ctx.Done():
			p.closeErr = errPage
			p.destroyTarget()
		}
	})
	return p.closeErr
}

func (p *pageDriver) Navigate(ctx context.Context, raw string) error {
	if p.closed.Load() || p.failed.Load() || !p.allowed(raw) {
		return errPage
	}
	p.targets, p.world = nil, 0
	p.blocked.Store(false)
	bounded, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	if err := chromedp.Run(bounded, chromedp.Navigate(raw)); err != nil || p.failed.Load() || p.blocked.Load() {
		return errPage
	}
	return nil
}

func (p *pageDriver) frameURL(ctx context.Context) (string, error) {
	var raw string
	err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		tree, err := page.GetFrameTree().Do(ctx)
		if err != nil || tree == nil || tree.Frame == nil || !p.allowed(tree.Frame.URL) {
			return errPage
		}
		p.frame, raw = tree.Frame.ID, tree.Frame.URL
		return nil
	}))
	if err != nil {
		return "", errPage
	}
	return safePageURL(raw), nil
}

func (p *pageDriver) evaluate(ctx context.Context, body string, arguments any, result any) error {
	if p.closed.Load() || p.failed.Load() || p.world == 0 {
		return errPage
	}
	encoded, err := json.Marshal(arguments)
	if err != nil {
		return errPage
	}
	script := "(function(args){" + safeDOM + body + "})(" + string(encoded) + ")"
	err = chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		value, exception, err := cdpruntime.Evaluate(script).WithContextID(p.world).WithReturnByValue(true).WithAwaitPromise(true).Do(ctx)
		if err != nil || exception != nil || value == nil {
			return errPage
		}
		if result != nil {
			return json.Unmarshal(value.Value, result)
		}
		return nil
	}))
	if err != nil || p.failed.Load() {
		return errPage
	}
	return nil
}

func (p *pageDriver) Observe(ctx context.Context) (Observation, error) {
	bounded, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	address, err := p.frameURL(bounded)
	if err != nil {
		return Observation{}, errPage
	}
	err = chromedp.Run(bounded, chromedp.ActionFunc(func(ctx context.Context) error {
		var err error
		p.world, err = page.CreateIsolatedWorld(p.frame).WithWorldName("commander-browser-observation").Do(ctx)
		return err
	}))
	if err != nil {
		return Observation{}, errPage
	}
	var observation Observation
	if err := p.evaluate(bounded, observeDOM, p.nextID, &observation); err != nil {
		return Observation{}, err
	}
	p.nextID += 60
	p.targets = make(map[int]string, len(observation.Targets))
	observation.URL, observation.Title, observation.Text = address, boundedPageText(observation.Title, 256), boundedPageText(observation.Text, 10000)
	if len(observation.Targets) > 60 {
		observation.Targets = observation.Targets[:60]
	}
	for i := range observation.Targets {
		item := &observation.Targets[i]
		item.Label, item.URL = boundedPageText(item.Label, 160), safePageURL(item.URL)
		p.targets[item.ID] = item.Kind
	}
	return observation, nil
}

func (p *pageDriver) Click(ctx context.Context, targetID int) error {
	if _, ok := p.targets[targetID]; !ok {
		return errPage
	}
	bounded, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	if _, err := p.frameURL(bounded); err != nil {
		return err
	}
	p.blocked.Store(false)
	if err := p.evaluate(bounded, clickDOM, map[string]any{"id": targetID, "hosts": p.hosts}, nil); err != nil || p.blocked.Load() {
		return errPage
	}
	p.targets = nil
	// Navigation may destroy the isolated world immediately after the synchronous
	// click. Settle outside that world and recreate it at the next observation.
	timer := time.NewTimer(100 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-bounded.Done():
		return errPage
	case <-timer.C:
	}
	if p.failed.Load() || p.blocked.Load() {
		return errPage
	}
	return nil
}

func (p *pageDriver) Type(ctx context.Context, targetID int, text string) error {
	kind := p.targets[targetID]
	if kind != "input" && kind != "textarea" || len(text) > 2000 {
		return errPage
	}
	bounded, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	if _, err := p.frameURL(bounded); err != nil {
		return err
	}
	if err := p.evaluate(bounded, typeDOM, map[string]any{"id": targetID, "text": text}, nil); err != nil {
		return err
	}
	p.targets = nil
	return nil
}

func (p *pageDriver) Extract(ctx context.Context, targetID int) (Evidence, error) {
	if targetID != 0 {
		if _, ok := p.targets[targetID]; !ok {
			return Evidence{}, errPage
		}
	}
	bounded, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	address, err := p.frameURL(bounded)
	if err != nil {
		return Evidence{}, err
	}
	var evidence Evidence
	if err := p.evaluate(bounded, extractDOM, targetID, &evidence); err != nil {
		return Evidence{}, err
	}
	evidence.URL, evidence.Title, evidence.Text = address, boundedPageText(evidence.Title, 256), boundedPageText(evidence.Text, 4000)
	return evidence, nil
}

func boundedPageText(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) > limit {
		value = value[:limit]
		for !utf8.ValidString(value) {
			value = value[:len(value)-1]
		}
	}
	return value
}

const safeDOM = `
const sensitive = /pass(?:word|wd)?|user(?:name)?|token|secret|credential|otp|one[-_ ]?time|api[-_ ]?key|cc[-_]|credit|card|cvc|cvv|auth|session|email/i;
function visible(el) {
  for (let depth=0; el && el.nodeType===1; el=el.parentElement) {
    if (++depth>64 || el.hidden || el.getAttribute('aria-hidden')==='true') return false;
    const s=getComputedStyle(el);
    if (s.display==='none' || s.visibility==='hidden' || s.visibility==='collapse' || s.opacity==='0') return false;
  }
  return true;
}
function fieldSafe(el) {
  return !sensitive.test((el.name||'')+' '+(el.id||'')+' '+(el.getAttribute('autocomplete')||''));
}
function kind(el) {
  if (!visible(el) || el.disabled || el.readOnly || !fieldSafe(el)) return '';
  const tag=el.tagName;
  if (tag==='TEXTAREA') return 'textarea';
  if (tag==='INPUT') {
    const type=(el.getAttribute('type')||'text').toLowerCase();
    if (type==='text' || type==='search') return 'input';
    if (type==='submit' || type==='button' || type==='checkbox' || type==='radio') return 'button';
    return '';
  }
  if (tag==='A' && el.hasAttribute('href') || el.getAttribute('role')==='link') return 'link';
  if (tag==='BUTTON' || el.getAttribute('role')==='button') return 'button';
  return '';
}
function textOf(root, limit) {
  const parts=[], stack=[root]; let used=0, visited=0;
  while (stack.length && visited++<5000 && used<limit) {
    const node=stack.pop();
    if (node.nodeType===3) {
      const text=(node.nodeValue||'').slice(0,limit-used).replace(/\s+/g,' ').trim();
      if (text) { parts.push(text); used+=text.length+1; }
    } else if (node.nodeType===1 && visible(node) && !/^(SCRIPT|STYLE|NOSCRIPT|TEMPLATE|INPUT|TEXTAREA|SELECT|OPTION)$/.test(node.tagName) && !node.isContentEditable) {
      const children=node.childNodes;
      for (let i=Math.min(children.length,200)-1;i>=0 && stack.length<6000;i--) stack.push(children[i]);
    }
  }
  return parts.join(' ').slice(0,limit);
}
function selected(id) {
  const el=globalThis.__commanderObserved && globalThis.__commanderObserved[id];
  if (!el || el.ownerDocument!==document || !document.contains(el) || !kind(el)) throw new Error('Unavailable target');
  return el;
}
`

const observeDOM = `
const targets=[], nodes=Object.create(null), stack=[document.body||document.documentElement]; let visited=0;
while (stack.length && visited++<5000 && targets.length<60) {
  const el=stack.pop();
  if (!el || el.nodeType!==1 || !visible(el) || /^(SCRIPT|STYLE|NOSCRIPT|TEMPLATE)$/.test(el.tagName)) continue;
  const type=kind(el);
  if (type) {
    const id=args+targets.length;
    let label=(el.getAttribute('aria-label')||el.getAttribute('title')||el.getAttribute('placeholder')||'').slice(0,160);
    if (!label && el.labels) for (const item of el.labels) { label=textOf(item,160); if (label) break; }
    if (!label) label=textOf(el,160);
    let url='';
    if (el.tagName==='A') { const href=el.getAttribute('href')||''; if (href.length<=2048) try { url=new URL(href,document.baseURI).href; } catch {} }
    targets.push({id,kind:type,label:label||type,url}); nodes[id]=el;
  }
  const children=el.children;
  for (let i=Math.min(children.length,200)-1;i>=0 && stack.length<6000;i--) stack.push(children[i]);
}
globalThis.__commanderObserved=nodes;
return {title:document.title.slice(0,256),text:textOf(document.body||document.documentElement,10000),targets};
`

const clickDOM = `
const el=selected(args.id);
function permitted(raw) {
  const u=new URL(raw,document.baseURI);
  return (u.protocol==='http:' || u.protocol==='https:') && !u.username && !u.password && (args.hosts['*'] || args.hosts[u.hostname.toLowerCase()]);
}
if (el.tagName==='A' && !permitted(el.href)) throw new Error('Unavailable navigation');
const baseTarget=document.querySelector('base[target]')?.target||'';
if (el.tagName==='A' && (el.target||baseTarget) && (el.target||baseTarget).toLowerCase()!=='_self') throw new Error('Unavailable browsing target');
if ((el.tagName==='BUTTON' || el.tagName==='INPUT') && el.form) {
  if (!permitted(el.hasAttribute('formaction') ? el.formAction : el.form.action)) throw new Error('Unavailable form');
  const formTarget=(el.hasAttribute('formtarget') ? el.formTarget : el.form.target)||baseTarget;
  if (formTarget && formTarget.toLowerCase()!=='_self') throw new Error('Unavailable browsing target');
}
el.click();
return true;
`

const typeDOM = `
const el=selected(args.id), type=kind(el);
if (type!=='input' && type!=='textarea') throw new Error('Unavailable field');
el.focus(); el.value=args.text;
el.dispatchEvent(new Event('input',{bubbles:true}));
el.dispatchEvent(new Event('change',{bubbles:true}));
return true;
`

const extractDOM = `
const el=args===0 ? document.body||document.documentElement : selected(args);
return {title:document.title.slice(0,256),text:textOf(el,4000)};
`
