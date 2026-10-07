package browser

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

// One warm process, at most four isolated incognito contexts. Proxy overrides
// use dedicated processes so transport settings cannot leak between callers.
type browserPool struct {
	mu    sync.Mutex
	root  context.Context
	close context.CancelFunc
	slots chan struct{}
}

var directBrowsers = browserPool{slots: make(chan struct{}, 4)}

func ClosePool() { directBrowsers.shutdown() }
func (p *browserPool) shutdown() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.close != nil {
		p.close()
	}
	p.root = nil
	p.close = nil
}

func (p *browserPool) acquire(ctx context.Context) (context.Context, context.CancelFunc, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	select {
	case p.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
	release := func() { <-p.slots }
	p.mu.Lock()
	if err := ctx.Err(); err != nil {
		p.mu.Unlock()
		release()
		return nil, nil, err
	}
	if p.root != nil {
		select {
		case <-chromedp.FromContext(p.root).Browser.LostConnection:
			p.close()
			p.root = nil
			p.close = nil
		default:
		}
	}
	if p.root == nil {
		alloc, stopAlloc := allocator(context.Background(), "")
		root, stopRoot := chromedp.NewContext(alloc)
		closeRoot := func() { stopRoot(); stopAlloc() }
		timer := time.AfterFunc(10*time.Second, closeRoot)
		err := chromedp.Run(root)
		timer.Stop()
		if err != nil {
			closeRoot()
			p.mu.Unlock()
			release()
			return nil, nil, fmt.Errorf("browser pool startup: %w", err)
		}
		p.root = root
		p.close = closeRoot
	}

	executor := cdp.WithExecutor(p.root, chromedp.FromContext(p.root).Browser)
	setupCtx, cancelSetup := context.WithTimeout(executor, 5*time.Second)
	stopSetup := context.AfterFunc(ctx, cancelSetup)
	defer func() { stopSetup(); cancelSetup() }()
	browserID, err := target.CreateBrowserContext().WithDisposeOnDetach(true).Do(setupCtx)
	if err != nil {
		p.mu.Unlock()
		release()
		return nil, nil, err
	}
	targetID, err := target.CreateTarget("about:blank").WithBrowserContextID(browserID).WithNewWindow(true).Do(setupCtx)
	if err != nil {
		_ = target.DisposeBrowserContext(browserID).Do(executor)
		p.mu.Unlock()
		release()
		return nil, nil, err
	}
	tab, cancelTab := chromedp.NewContext(p.root, chromedp.WithTargetID(targetID))
	dispose := func() {
		cleanupCtx, cancel := context.WithTimeout(executor, 3*time.Second)
		defer cancel()
		_ = target.DisposeBrowserContext(browserID).Do(cleanupCtx)
	}

	p.mu.Unlock()
	if err := ctx.Err(); err != nil {
		cancelTab()
		dispose()
		release()
		return nil, nil, err
	}
	stop := context.AfterFunc(ctx, cancelTab)
	var once sync.Once
	cleanup := func() { once.Do(func() { stop(); cancelTab(); dispose(); release() }) }
	return tab, cleanup, nil
}

func browserContext(ctx context.Context, proxy string) (context.Context, context.CancelFunc, error) {
	if proxy == "" || proxy == "-" {
		return directBrowsers.acquire(ctx)
	}
	alloc, stopAlloc := allocator(ctx, proxy)
	tab, stopTab := chromedp.NewContext(alloc)
	return tab, func() { stopTab(); stopAlloc() }, nil
}
