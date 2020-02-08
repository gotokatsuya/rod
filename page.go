package rod

import (
	"context"
	"encoding/base64"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ysmood/kit"
	"github.com/ysmood/rod/lib/cdp"
)

// Page represents the webpage
type Page struct {
	ctx     context.Context
	browser *Browser

	TargetID  string
	SessionID string
	ContextID int64

	// devices
	Mouse    *Mouse
	Keyboard *Keyboard

	// iframe only
	FrameID string
	element *Element

	timeoutCancel       func()
	getDownloadFileLock *sync.Mutex
}

// Ctx sets the context for chained sub-operations
func (p *Page) Ctx(ctx context.Context) *Page {
	newObj := *p
	newObj.ctx = ctx
	return &newObj
}

// Timeout sets the timeout for chained sub-operations
func (p *Page) Timeout(d time.Duration) *Page {
	ctx, cancel := context.WithTimeout(p.ctx, d)
	p.timeoutCancel = cancel
	return p.Ctx(ctx)
}

// CancelTimeout ...
func (p *Page) CancelTimeout() {
	if p.timeoutCancel != nil {
		p.timeoutCancel()
	}
}

// NavigateE ...
func (p *Page) NavigateE(url string) error {
	_, err := p.Call("Page.navigate", cdp.Object{
		"url": url,
	})
	return err
}

// Navigate to url
func (p *Page) Navigate(url string) *Page {
	kit.E(p.NavigateE(url))
	return p
}

// SetViewportE ...
// Prams: https://chromedevtools.github.io/devtools-protocol/tot/Emulation#method-setDeviceMetricsOverride
func (p *Page) SetViewportE(params *cdp.Object) error {
	if params == nil {
		return nil
	}
	_, err := p.Call("Emulation.setDeviceMetricsOverride", params)
	return err
}

// SetViewport overrides the values of device screen dimensions.
func (p *Page) SetViewport(width, height int, deviceScaleFactor float32, mobile bool) {
	kit.E(p.SetViewportE(&cdp.Object{
		"width":             width,
		"height":            height,
		"deviceScaleFactor": deviceScaleFactor,
		"mobile":            mobile,
	}))
}

// CloseE page
func (p *Page) CloseE() error {
	_, err := p.Call("Page.close", nil)
	return err
}

// Close page
func (p *Page) Close() {
	kit.E(p.CloseE())
}

// HasE ...
func (p *Page) HasE(selector string) (bool, error) {
	res, err := p.EvalE(true, "", `s => !!document.querySelector(s)`, []interface{}{selector})
	if err != nil {
		return false, err
	}
	return res.Bool(), nil
}

// Has an element that matches the css selector
func (p *Page) Has(selector string) bool {
	has, err := p.HasE(selector)
	kit.E(err)
	return has
}

// ElementE ...
func (p *Page) ElementE(selector string) (*Element, error) {
	return p.ElementByJSE(p.Sleeper(), "", `s => document.querySelector(s)`, []interface{}{selector})
}

// Element retries until returns the first element in the page that matches the selector
func (p *Page) Element(selector string) *Element {
	el, err := p.ElementE(selector)
	kit.E(err)
	return el
}

// Sleeper returns the default sleeper for retry, it will wake whenever Page for DOM event happens,
// and use backoff as the backup to wake.
func (p *Page) Sleeper() kit.Sleeper {
	backoff := kit.BackoffSleeper(30*time.Millisecond, 3*time.Second, nil)

	return kit.MergeSleepers(backoff, func(ctx context.Context) error {
		s := p.browser.event.Subscribe()
		defer p.browser.event.Unsubscribe(s)
		prefix := strings.HasPrefix

		c := s.Filter(func(e kit.Event) bool {
			m := e.(*cdp.Message).Method
			if prefix(m, "Page") || prefix(m, "DOM") {
				return true
			}
			return false
		})

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c:
		}
		return nil
	})
}

// ElementByJSE returns the element from the return value of the js function.
// sleeper is used to sleep before retry the operation.
// thisID is the this value of the js function, when thisID is "", the this context will be the "window".
// If the js function returns "null", ElementByJSE will retry, you can use custom sleeper to make it only
// retries once.
func (p *Page) ElementByJSE(sleeper kit.Sleeper, thisID, js string, params []interface{}) (*Element, error) {
	var val kit.JSONResult

	err := kit.Retry(p.ctx, sleeper, func() (bool, error) {
		res, err := p.EvalE(false, thisID, js, params)
		if err != nil {
			return true, err
		}
		v := res.Get("result")
		val = &v

		if val.Get("type").String() == "object" && val.Get("subtype").String() == "null" {
			return false, nil
		}

		return true, nil
	})
	if err != nil {
		return nil, err
	}

	if val.Get("subtype").String() != "node" {
		return nil, &Error{nil, ErrExpectElement, val.Raw}
	}

	return &Element{
		page:     p,
		ctx:      p.ctx,
		ObjectID: val.Get("objectId").String(),
	}, nil
}

// ElementByJS retries until returns the element from the return value of the js function
func (p *Page) ElementByJS(js string, params ...interface{}) *Element {
	el, err := p.ElementByJSE(p.Sleeper(), "", js, params)
	kit.E(err)
	return el
}

// ElementsE ...
func (p *Page) ElementsE(selector string) ([]*Element, error) {
	return p.ElementsByJSE("", `s => document.querySelectorAll(s)`, []interface{}{selector})
}

// Elements returns all elements that match the selector
func (p *Page) Elements(selector string) []*Element {
	list, err := p.ElementsE(selector)
	kit.E(err)
	return list
}

// ElementsByJSE is different from ElementByJSE, it doesn't do retry
func (p *Page) ElementsByJSE(thisID, js string, params []interface{}) ([]*Element, error) {
	res, err := p.EvalE(false, thisID, js, params)
	if err != nil {
		return nil, err
	}
	val := res.Get("result")

	if val.Get("subtype").String() != "array" {
		return nil, &Error{nil, ErrExpectElements, val}
	}

	objectID := val.Get("objectId").String()
	if objectID == "" {
		return []*Element{}, nil
	}
	defer p.ReleaseObject(res)

	list, err := p.Call("Runtime.getProperties", cdp.Object{
		"objectId":      objectID,
		"ownProperties": true,
	})
	if err != nil {
		return nil, err
	}

	elemList := []*Element{}
	for _, obj := range list.Get("result").Array() {
		name := obj.Get("name").String()
		if name == "__proto__" || name == "length" {
			continue
		}
		val := obj.Get("value")

		if val.Get("subtype").String() != "node" {
			return nil, &Error{nil, ErrExpectElements, val}
		}

		elemList = append(elemList, &Element{
			page:     p,
			ctx:      p.ctx,
			ObjectID: val.Get("objectId").String(),
		})
	}

	return elemList, nil
}

// ElementsByJS returns the elements from the return value of the js
func (p *Page) ElementsByJS(js string, params ...interface{}) []*Element {
	list, err := p.ElementsByJSE("", js, params)
	kit.E(err)
	return list
}

// HandleDialogE ...
func (p *Page) HandleDialogE(accept bool, promptText string) error {
	_, err := p.browser.Ctx(p.ctx).WaitEventE("Page.javascriptDialogOpening")
	if err != nil {
		return err
	}
	_, err = p.Call("Page.handleJavaScriptDialog", cdp.Object{
		"accept":     accept,
		"promptText": promptText,
	})
	return err
}

// HandleDialog accepts or dismisses next JavaScript initiated dialog (alert, confirm, prompt, or onbeforeunload)
func (p *Page) HandleDialog(accept bool, promptText string) {
	kit.E(p.HandleDialogE(accept, promptText))
}

// GetDownloadFileE how it works is to proxy the request, the dir is the dir to save the file.
func (p *Page) GetDownloadFileE(dir, pattern string) (http.Header, []byte, error) {
	var params cdp.Object
	if pattern != "" {
		params = cdp.Object{
			"patterns": []cdp.Object{
				{"urlPattern": pattern},
			},
		}
	}

	// both Page.setDownloadBehavior and Fetch.enable will pollute the global status,
	// we have to prevent race condition here
	p.getDownloadFileLock.Lock()
	defer p.getDownloadFileLock.Unlock()

	_, err := p.Call("Page.setDownloadBehavior", cdp.Object{
		"behavior":     "allow",
		"downloadPath": dir,
	})
	if err != nil {
		return nil, nil, err
	}

	_, err = p.Call("Fetch.enable", params)
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		_, err = p.Call("Fetch.disable", nil)
	}()

	msg, err := p.browser.Ctx(p.ctx).WaitEventE("Fetch.requestPaused")
	if err != nil {
		return nil, nil, err
	}

	msgReq := msg.Get("request")
	req := kit.Req(msgReq.Get("url").String())

	for k, v := range msgReq.Get("headers").Map() {
		req.Header(k, v.String())
	}

	res, err := req.Response()
	if err != nil {
		return nil, nil, err
	}

	body, err := req.Bytes()
	if err != nil {
		return nil, nil, err
	}

	headers := []cdp.Object{}
	for k, vs := range res.Header {
		for _, v := range vs {
			headers = append(headers, cdp.Object{
				"name":  k,
				"value": v,
			})
		}
	}

	_, err = p.Call("Fetch.fulfillRequest", cdp.Object{
		"requestId":       msg.Get("requestId").String(),
		"responseCode":    res.StatusCode,
		"responseHeaders": headers,
		"body":            base64.StdEncoding.EncodeToString(body),
	})

	return res.Header, body, err
}

// GetDownloadFile of the next download url that matches the pattern, returns the response header and file content.
// Wildcards ('*' -> zero or more, '?' -> exactly one) are allowed. Escape character is backslash. Omitting is equivalent to "*".
func (p *Page) GetDownloadFile(pattern string) (http.Header, []byte) {
	h, f, err := p.GetDownloadFileE(filepath.FromSlash("tmp/rod-downloads"), pattern)
	kit.E(err)
	return h, f
}

// ScreenshopE options: https://chromedevtools.github.io/devtools-protocol/tot/Page#method-captureScreenshot
func (p *Page) ScreenshopE(options cdp.Object) ([]byte, error) {
	res, err := p.Call("Page.captureScreenshot", options)
	if err != nil {
		return nil, err
	}
	return base64.StdEncoding.DecodeString(res.Get("data").String())
}

// Screenshop the page
func (p *Page) Screenshop() []byte {
	png, err := p.ScreenshopE(nil)
	kit.E(err)
	return png
}

// WaitPageE ...
func (p *Page) WaitPageE() (*Page, error) {
	var targetInfo cdp.Object

	_, err := p.browser.event.Until(p.ctx, func(e kit.Event) bool {
		msg := e.(*cdp.Message)
		if msg.Method == "Target.targetCreated" {
			targetInfo = msg.Params.(map[string]interface{})["targetInfo"].(map[string]interface{})

			if targetInfo["openerId"] == p.TargetID {
				return true
			}
		}
		return false
	})

	if err != nil {
		return nil, err
	}

	return p.browser.page(targetInfo["targetId"].(string))
}

// WaitPage to be opened from the specified page
func (p *Page) WaitPage() *Page {
	newPage, err := p.WaitPageE()
	kit.E(err)
	return newPage
}

// PauseE ...
func (p *Page) PauseE() error {
	_, err := p.Call("Debugger.enable", nil)
	if err != nil {
		return err
	}
	_, err = p.Call("Debugger.pause", nil)
	if err != nil {
		return err
	}
	_, err = p.browser.Ctx(p.ctx).WaitEventE("Debugger.resumed")
	return err
}

// Pause stops on the next JavaScript statement
func (p *Page) Pause() {
	kit.E(p.PauseE())
}

// EvalE thisID is the remote objectID that will be the this of the js function
func (p *Page) EvalE(byValue bool, thisID, js string, jsArgs []interface{}) (res kit.JSONResult, err error) {
	if thisID == "" {
		res, err = p.eval(byValue, js, jsArgs)
	} else {
		res, err = p.evalThis(byValue, thisID, js, jsArgs)
	}

	if err != nil {
		return nil, err
	}

	if res.Get("exceptionDetails").Exists() {
		return nil, &Error{nil, res.Get("exceptionDetails.exception.description").String(), res}
	}

	if byValue {
		val := res.Get("result.value")
		res = &val
	}

	return
}

func (p *Page) eval(byValue bool, js string, jsArgs []interface{}) (kit.JSONResult, error) {
	params := cdp.Object{
		"expression":    SprintFnApply(js, jsArgs),
		"awaitPromise":  true,
		"returnByValue": byValue,
	}
	if p.isIframe() {
		return p.evalIframe(params)
	}
	return p.Call("Runtime.evaluate", params)
}

func (p *Page) evalIframe(params cdp.Object) (res kit.JSONResult, err error) {
	backoff := kit.BackoffSleeper(30*time.Millisecond, 3*time.Second, nil)
	// TODO: ContextID will be invalid if a frame is reloaded
	// For now I don't know a better way to do it other than retry
	err = kit.Retry(p.ctx, backoff, func() (bool, error) {
		params["contextId"] = p.ContextID
		res, err = p.Call("Runtime.evaluate", params)

		if cdpErr, ok := err.(*cdp.Error); ok && cdpErr.Code == -32000 {
			_ = p.initIsolatedWorld()
			return false, nil
		}

		return true, err
	})
	return
}

func (p *Page) evalThis(byValue bool, thisID, js string, jsArgs []interface{}) (kit.JSONResult, error) {
	args := []interface{}{}
	for _, p := range jsArgs {
		args = append(args, cdp.Object{"value": p})
	}

	params := cdp.Object{
		"objectId":            thisID,
		"awaitPromise":        true,
		"returnByValue":       byValue,
		"functionDeclaration": SprintFnThis(js),
		"arguments":           args,
	}

	return p.Call("Runtime.callFunctionOn", params)
}

// Eval js under sessionID or contextId, if contextId doesn't exist create a new isolatedWorld.
// The first param must be a js function definition.
// For example: page.Eval(`s => document.querySelectorAll(s)`, "input")
func (p *Page) Eval(js string, params ...interface{}) kit.JSONResult {
	res, err := p.EvalE(true, "", js, params)
	kit.E(err)
	return res
}

// Call sends a control message to the browser with the page session, the call is always on the root frame.
func (p *Page) Call(method string, params interface{}) (kit.JSONResult, error) {
	return p.browser.Ctx(p.ctx).Call(&cdp.Message{
		SessionID: p.SessionID,
		Method:    method,
		Params:    params,
	})
}

// ReleaseObject remote object
func (p *Page) ReleaseObject(obj kit.JSONResult) {
	_, err := p.Call("Runtime.releaseObject", cdp.Object{
		"objectId": obj.Get("result.objectId").String(),
	})
	if err != nil {
		p.browser.fatal.Publish(err)
	}
}

func (p *Page) initIsolatedWorld() error {
	frame, err := p.Call("Page.createIsolatedWorld", cdp.Object{
		"frameId": p.FrameID,
	})
	if err != nil {
		return err
	}

	p.ContextID = frame.Get("executionContextId").Int()
	return nil
}

func (p *Page) initSession() error {
	obj, err := p.Call("Target.attachToTarget", cdp.Object{
		"targetId": p.TargetID,
		"flatten":  true, // if it's not set no response will return
	})
	if err != nil {
		return err
	}
	p.SessionID = obj.Get("sessionId").String()
	_, err = p.Call("Page.enable", nil)
	if err != nil {
		return err
	}
	return p.SetViewportE(p.browser.Viewport)
}

func (p *Page) isIframe() bool {
	return p.FrameID != ""
}

func (p *Page) rootFrame() *Page {
	f := p

	for f.isIframe() {
		f = f.element.page
	}

	return f
}
