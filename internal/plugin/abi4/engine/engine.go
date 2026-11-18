package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"wasmgate/internal/plugin/abi4/wire"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

const (
	hdrLen     = wire.HeaderLen
	maxEntries = wire.MaxEntries
	maxU32     = wire.MaxU32

	// DefaultMaxMemoryPages caps linear memory per instance. Reference guests
	// built with plugins/examples/.cargo/config.toml request one or two pages,
	// but the cap stays at 17 so modules built with Rust's default 1 MiB stack
	// also load without a per-listener override.
	DefaultMaxMemoryPages = 17
	DefaultHookTimeout    = time.Second
	DefaultLoadTimeout    = 5 * time.Second
	DefaultInCap          = 40 * 1024
	DefaultOutCap         = 4 * 1024
	maxIdleInstances      = 16
	// DefaultMaxInstances bounds concurrent live instances per engine. Idle
	// instances are capped separately. Each instance can reserve up to
	// MaxMemoryPages of linear memory, so this is a hard memory ceiling under
	// concurrent load rather than a tuning knob for throughput alone.
	DefaultMaxInstances = 64
	// MaxInstancesLimit is the largest accepted MaxInstances value. It keeps a
	// misconfigured caller from allocating an absurd semaphore or a memory
	// ceiling far beyond the host's budget.
	MaxInstancesLimit = 1024
	// DefaultHeaderIndexCap bounds the pooled header index used by
	// OnRequestHeaders. 512 entries is already far above real-world HTTP
	// requests and keeps the per-event buffer at 8 KiB.
	DefaultHeaderIndexCap = 512
)

// Options configures the v4 engine. Plan is required: it is the explicit
// contract between the listener and the plugin.
type Options struct {
	MaxMemoryPages uint32
	Timeout        time.Duration
	Plan           Plan
	// RequiredCaps adds policy-specific requirements to Plan.RequiredCaps.
	RequiredCaps Caps
	InCap        int
	OutCap       int
	RoutePools   int
	// HeaderIndexCap is the maximum number of headers accepted by
	// OnRequestHeaders. Zero selects DefaultHeaderIndexCap.
	HeaderIndexCap int
	// MaxInstances bounds concurrent borrowed instances. Zero selects
	// DefaultMaxInstances. Callers beyond the limit wait for a free slot or
	// for their context/load deadline to expire.
	MaxInstances int
}

type instance struct {
	mod       api.Module
	onEvent   api.Function
	onAccept  api.Function
	onClose   api.Function
	onCloseEx api.Function
	mem       api.Memory
	bufPtr    uint32
	outPtr    uint32
	outLen    uint32
	abi       uint32
	caps      Caps
	stack     [5]uint64
}

// Engine is a compiled v4 module plus a pool of reusable instances. It is safe
// for concurrent event calls; Close must not race with in-flight events. After
// warm-up, payload writes and header indexing avoid allocations. Hook deadlines
// allocate a context per event; accepted OUT effects allocate host-owned bytes.
type Engine struct {
	rt          wazero.Runtime
	compiled    wazero.CompiledModule
	plan        Plan
	required    Caps
	caps        Caps
	abi         uint32
	inCap       uint32
	outCap      uint32
	timeout     time.Duration
	loadTimeout time.Duration
	routePools  int
	headerCap   int
	hasAccept   bool
	hasClose    bool
	hasCloseEx  bool
	nextID      atomic.Uint64
	discarded   atomic.Int64
	refs        atomic.Int64
	pool        chan *instance
	sem         chan struct{}
	idxPool     sync.Pool
	closeOnce   sync.Once
}

// LoadFile reads and compiles a v4 module.
func LoadFile(ctx context.Context, path string, opts Options) (*Engine, error) {
	wasmBytes, err := os.ReadFile(path) // #nosec G304 -- path comes from validated config
	if err != nil {
		return nil, fmt.Errorf("abi4: read %s: %w", path, err)
	}
	return New(ctx, wasmBytes, opts)
}

// New compiles a v4 module and validates its ABI, capabilities, window and
// on_event contract before serving any event.
func New(ctx context.Context, wasmBytes []byte, opts Options) (*Engine, error) {
	if len(wasmBytes) == 0 {
		return nil, errors.New("abi4: empty wasm binary")
	}
	if err := opts.Plan.Validate(); err != nil {
		return nil, err
	}
	if opts.InCap == 0 {
		opts.InCap = DefaultInCap
	}
	if opts.OutCap == 0 {
		opts.OutCap = DefaultOutCap
	}
	if opts.InCap < hdrLen || opts.OutCap <= 0 ||
		uint64(opts.InCap)+uint64(opts.OutCap) > maxU32 {
		return nil, errors.New("abi4: invalid window capacities")
	}
	if opts.HeaderIndexCap == 0 {
		opts.HeaderIndexCap = DefaultHeaderIndexCap
	}
	if opts.HeaderIndexCap < 1 || opts.HeaderIndexCap > maxEntries {
		return nil, fmt.Errorf("abi4: header index cap must be between 1 and %d", maxEntries)
	}
	if opts.MaxInstances == 0 {
		opts.MaxInstances = DefaultMaxInstances
	} else if opts.MaxInstances < 1 || opts.MaxInstances > MaxInstancesLimit {
		return nil, fmt.Errorf("abi4: max instances must be between 1 and %d", MaxInstancesLimit)
	}
	if opts.MaxMemoryPages == 0 {
		opts.MaxMemoryPages = DefaultMaxMemoryPages
	} else if opts.MaxMemoryPages > 65536 {
		return nil, errors.New("abi4: memory limit exceeds Wasm32 address space")
	}
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultHookTimeout
	}
	if opts.RoutePools < 0 {
		opts.RoutePools = 0
	}

	loadCtx, cancel := context.WithTimeout(ctx, DefaultLoadTimeout)
	defer cancel()

	rt := wazero.NewRuntimeWithConfig(loadCtx,
		wazero.NewRuntimeConfig().WithMemoryLimitPages(opts.MaxMemoryPages).WithCloseOnContextDone(true))
	compiled, err := rt.CompileModule(loadCtx, wasmBytes)
	if err != nil {
		_ = rt.Close(loadCtx)
		return nil, fmt.Errorf("abi4: compile: %w", err)
	}
	if err := validateExports(compiled, opts.Plan); err != nil {
		_ = rt.Close(loadCtx)
		return nil, err
	}

	e := &Engine{
		rt:          rt,
		compiled:    compiled,
		plan:        opts.Plan,
		required:    opts.Plan.RequiredCaps() | opts.RequiredCaps,
		inCap:       uint32(opts.InCap),  // #nosec G115 -- positive config value
		outCap:      uint32(opts.OutCap), // #nosec G115 -- positive config value
		timeout:     opts.Timeout,
		loadTimeout: DefaultLoadTimeout,
		routePools:  opts.RoutePools,
		headerCap:   opts.HeaderIndexCap,
		pool:        make(chan *instance, maxIdleInstances),
		sem:         make(chan struct{}, opts.MaxInstances),
	}
	e.refs.Store(1)
	warm, err := e.instantiate(loadCtx)
	if err != nil {
		_ = rt.Close(loadCtx)
		return nil, err
	}
	e.abi = warm.abi
	e.caps = warm.caps
	e.hasAccept = warm.onAccept != nil
	e.hasClose = warm.onClose != nil
	e.hasCloseEx = warm.onCloseEx != nil

	e.idxPool.New = func() any {
		buf := make([]Ent, e.headerCap)
		return &buf
	}
	e.pool <- warm
	runtime.AddCleanup(e, func(rt wazero.Runtime) {
		_ = rt.Close(context.Background())
	}, e.rt)
	return e, nil
}
