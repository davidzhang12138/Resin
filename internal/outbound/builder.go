package outbound

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync/atomic"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/endpoint"
	"github.com/sagernet/sing-box/adapter/inbound"
	sbOutbound "github.com/sagernet/sing-box/adapter/outbound"
	"github.com/sagernet/sing-box/common/dialer"
	"github.com/sagernet/sing-box/dns"
	"github.com/sagernet/sing-box/include"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common"
	sJson "github.com/sagernet/sing/common/json"
	M "github.com/sagernet/sing/common/metadata"
	"github.com/sagernet/sing/service"
	"github.com/sagernet/sing/service/pause"
)

// OutboundResolverFunc resolves an outbound by its tag from an external source (e.g. node pool).
type OutboundResolverFunc func(tag string) (adapter.Outbound, bool)

// OutboundResolverSetter allows setting an external outbound resolver for detour chaining.
type OutboundResolverSetter interface {
	SetOutboundResolver(resolver OutboundResolverFunc)
}

type detourOutboundManager struct {
	*sbOutbound.Manager
	resolver atomic.Pointer[OutboundResolverFunc]
}

func (m *detourOutboundManager) Outbound(tag string) (adapter.Outbound, bool) {
	if ob, ok := m.Manager.Outbound(tag); ok {
		return ob, true
	}
	resFn := m.resolver.Load()
	if resFn != nil && *resFn != nil {
		ob, ok := (*resFn)(tag)
		if ok && ob != nil {
			switch ob.Type() {
			case "fallback", "urltest", "selector":
				// sing-box caches detour objects. Keep the group reference live
				// across subscription replacement instead of retaining a closed group.
				return &liveDetourGroup{Outbound: ob, resolve: *resFn}, true
			}
		}
		return ob, ok
	}
	return nil, false
}

type liveDetourGroup struct {
	adapter.Outbound
	resolve OutboundResolverFunc
}

func (g *liveDetourGroup) current() (adapter.Outbound, error) {
	ob, ok := g.resolve(g.Tag())
	if !ok || ob == nil {
		return nil, fmt.Errorf("outbound detour not found: %s", g.Tag())
	}
	return ob, nil
}

func (g *liveDetourGroup) DialContext(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
	ob, err := g.current()
	if err != nil {
		return nil, err
	}
	return ob.DialContext(ctx, network, destination)
}

func (g *liveDetourGroup) ListenPacket(ctx context.Context, destination M.Socksaddr) (net.PacketConn, error) {
	ob, err := g.current()
	if err != nil {
		return nil, err
	}
	return ob.ListenPacket(ctx, destination)
}

func (g *liveDetourGroup) Now() string {
	ob, err := g.current()
	if err != nil {
		return ""
	}
	if group, ok := ob.(adapter.OutboundGroup); ok {
		return group.Now()
	}
	return ob.Tag()
}

func (g *liveDetourGroup) All() []string {
	ob, err := g.current()
	if err != nil {
		return nil
	}
	if group, ok := ob.(adapter.OutboundGroup); ok {
		return group.All()
	}
	return []string{ob.Tag()}
}

// OutboundBuilder creates outbound instances from raw node options.
type OutboundBuilder interface {
	Build(rawOptions json.RawMessage) (adapter.Outbound, error)
}

// SingboxBuilderConfig configures SingboxBuilder construction.
type SingboxBuilderConfig struct {
	// DNSUpstreams configures Resin's node DNS chain.
	// Values are DNS upstream URI strings and the slice must not be empty.
	DNSUpstreams []string
}

// ---------------------------------------------------------------------------
// SingboxBuilder — creates real sing-box adapter.Outbound instances.
// ---------------------------------------------------------------------------

// SingboxBuilder builds real sing-box outbound instances from raw JSON options.
// It holds a fully-wired context with DNS services so that domain-based
// outbound servers can be resolved.
type SingboxBuilder struct {
	registry            *sbOutbound.Registry
	ctx                 context.Context
	logFactory          log.Factory
	dnsTransportManager *dns.TransportManager
	dnsRouter           *dns.Router
	detourMgr           *detourOutboundManager
}

// SetOutboundResolver configures the external outbound resolver for detour chaining.
func (b *SingboxBuilder) SetOutboundResolver(resolver OutboundResolverFunc) {
	if b.detourMgr != nil {
		b.detourMgr.resolver.Store(&resolver)
	}
}

// NewSingboxBuilderWithConfig creates a SingboxBuilder with a complete
// sing-box service graph (registries + DNS). The caller must call Close()
// when done.
func NewSingboxBuilderWithConfig(cfg SingboxBuilderConfig) (*SingboxBuilder, error) {
	ctx := context.Background()
	ctx = include.Context(ctx) // inject protocol registries
	ctx = pause.WithDefaultManager(ctx)
	registerFallback(service.FromContext[adapter.OutboundRegistry](ctx).(*sbOutbound.Registry))

	logFactory := log.NewNOPFactory()
	logger := logFactory.NewLogger("resin-outbound")

	dnsRegistry, ok := service.FromContext[adapter.DNSTransportRegistry](ctx).(*dns.TransportRegistry)
	if !ok {
		return nil, fmt.Errorf("singbox builder: unexpected DNS transport registry type %T", service.FromContext[adapter.DNSTransportRegistry](ctx))
	}
	registerSecureDNSTransport(dnsRegistry)

	// --- Service graph (same order as Demos/simple-proxy/main.go) -----------

	// Endpoint Manager
	endpointMgr := endpoint.NewManager(logger, service.FromContext[adapter.EndpointRegistry](ctx))
	service.MustRegister[adapter.EndpointManager](ctx, endpointMgr)

	// Inbound Manager (required dependency even though unused)
	inboundMgr := inbound.NewManager(logger, service.FromContext[adapter.InboundRegistry](ctx), endpointMgr)
	service.MustRegister[adapter.InboundManager](ctx, inboundMgr)

	// Outbound Manager (sing-box's own manager, wrapped for detour resolution)
	rawOutboundMgr := sbOutbound.NewManager(logger, service.FromContext[adapter.OutboundRegistry](ctx), endpointMgr, "")
	detourMgr := &detourOutboundManager{Manager: rawOutboundMgr}
	service.MustRegister[adapter.OutboundManager](ctx, detourMgr)

	// DNS Transport Manager
	dnsTransportMgr := dns.NewTransportManager(logger, service.FromContext[adapter.DNSTransportRegistry](ctx), detourMgr, secureDNSFailoverTransportTag)
	service.MustRegister[adapter.DNSTransportManager](ctx, dnsTransportMgr)

	// DNS Router
	dnsRouter := dns.NewRouter(ctx, logFactory, option.DNSOptions{})
	service.MustRegister[adapter.DNSRouter](ctx, dnsRouter)

	dnsTransportSpecs, err := secureDNSTransportSpecsForUpstreams(cfg.DNSUpstreams)
	if err != nil {
		return nil, fmt.Errorf("singbox builder: configure DNS transports: %w", err)
	}
	for _, spec := range dnsTransportSpecs {
		if err := dnsTransportMgr.Create(ctx, logger, spec.tag, spec.transportType, spec.options); err != nil {
			return nil, fmt.Errorf("singbox builder: create DNS transport %s[%s]: %w", spec.transportType, spec.tag, err)
		}
	}

	// Start DNS Transport Manager lifecycle
	if err := dnsTransportMgr.Start(adapter.StartStateInitialize); err != nil {
		return nil, fmt.Errorf("singbox builder: initialize DNS transport manager: %w", err)
	}
	if err := dnsTransportMgr.Start(adapter.StartStateStart); err != nil {
		_ = dnsTransportMgr.Close()
		return nil, fmt.Errorf("singbox builder: start DNS transport manager: %w", err)
	}

	// Start DNS Router lifecycle
	if err := dnsRouter.Initialize(nil); err != nil {
		_ = dnsTransportMgr.Close()
		return nil, fmt.Errorf("singbox builder: initialize DNS router: %w", err)
	}
	if err := dnsRouter.Start(adapter.StartStateStart); err != nil {
		_ = dnsRouter.Close()
		_ = dnsTransportMgr.Close()
		return nil, fmt.Errorf("singbox builder: start DNS router: %w", err)
	}

	registry := service.FromContext[adapter.OutboundRegistry](ctx).(*sbOutbound.Registry)

	return &SingboxBuilder{
		registry:            registry,
		ctx:                 ctx,
		logFactory:          logFactory,
		dnsTransportManager: dnsTransportMgr,
		dnsRouter:           dnsRouter,
		detourMgr:           detourMgr,
	}, nil
}

// Build parses rawOptions (a complete sing-box outbound JSON object with
// type/tag fields) into a real adapter.Outbound and runs it through the
// lifecycle stages.
func (b *SingboxBuilder) Build(rawOptions json.RawMessage) (adapter.Outbound, error) {
	return b.build(rawOptions, nil)
}

func (b *SingboxBuilder) BuildWithDetour(rawOptions json.RawMessage, upstream adapter.Outbound) (adapter.Outbound, error) {
	return b.build(rawOptions, upstream)
}

func (b *SingboxBuilder) build(rawOptions json.RawMessage, upstream adapter.Outbound) (adapter.Outbound, error) {
	ctx := b.ctx
	// 1. Parse via official option.Outbound path (strips type/tag, creates
	//    typed options via OutboundOptionsRegistry + badjson.UnmarshallExcluded).
	var outboundConfig option.Outbound
	if err := sJson.UnmarshalContext(b.ctx, rawOptions, &outboundConfig); err != nil {
		return nil, fmt.Errorf("parse outbound options: %w", err)
	}
	options, canDetour := outboundConfig.Options.(option.DialerOptionsWrapper)
	originalDetour := ""
	if canDetour {
		// Save the subscription's detour before the chain/observe logic
		// replaces it with resinDetourTag.
		originalDetour = options.TakeDialerOptions().Detour
	}
	observeServer := false
	if upstream == nil && canDetour && originalDetour == "" {
		// Observe server TCP dials through the same scoped detour mechanism.
		// Packet transports retain their native dialer defaults and lifecycle.
		switch outboundConfig.Type {
		case "socks", "http", "ssh", "shadowsocks", "vmess", "vless", "trojan", "anytls":
			observeServer = true
		}
	}
	if upstream != nil || observeServer {
		if !canDetour {
			return nil, fmt.Errorf("outbound %s does not support detour", outboundConfig.Type)
		}
		ctx = service.ContextWithRegistry(ctx, &outboundRegistryScope{
			local: service.NewRegistry(), parent: service.RegistryFromContext(b.ctx),
		})
		dial := options.TakeDialerOptions()
		if observeServer {
			service.MustRegister[adapter.DNSRouter](ctx, &serverDNSRouter{DNSRouter: service.FromContext[adapter.DNSRouter](ctx)})
			server := outboundConfig.Options.(option.ServerOptionsWrapper).TakeServerOptions()
			base, err := dialer.New(ctx, dial, server.ServerIsDomain())
			if err != nil {
				return nil, fmt.Errorf("create server dialer: %w", err)
			}
			upstream = &serverDialer{Adapter: newChainAdapter(), Dialer: base}
			// The wrapped original dialer already resolves the server. Resolving
			// outside it would lose attribution and change address fallback.
			dial.DomainResolver = nil
		}
		dial.Detour = resinDetourTag
		options.ReplaceDialerOptions(dial)
		service.MustRegister[adapter.OutboundManager](ctx, &detourManager{
			OutboundManager: service.FromContext[adapter.OutboundManager](b.ctx), upstream: upstream,
		})
	}

	// 2. Create the outbound instance via the registry.
	logger := b.logFactory.NewLogger("outbound/" + outboundConfig.Type)
	ob, err := b.registry.CreateOutbound(
		ctx,
		nil, // router — not needed for simple dialing
		logger,
		outboundConfig.Tag,
		outboundConfig.Type,
		outboundConfig.Options,
	)
	if err != nil {
		return nil, fmt.Errorf("create outbound [%s]: %w", outboundConfig.Type, err)
	}

	// 3. Run lifecycle start stages. On failure, close and return error.
	for _, stage := range adapter.ListStartStages {
		if err := adapter.LegacyStart(ob, stage); err != nil {
			_ = common.Close(ob)
			return nil, fmt.Errorf("outbound start %s [%s]: %w", stage, outboundConfig.Type, err)
		}
	}

	// 4. Eagerly resolve detour dependencies so a broken detour fails the build
	// instead of failing every dial. Chain nodes resolve their relay separately.
	if b.detourMgr != nil && originalDetour != "" {
		for _, depTag := range ob.Dependencies() {
			if _, ok := b.detourMgr.Outbound(depTag); !ok {
				_ = common.Close(ob)
				return nil, fmt.Errorf("outbound detour not found: %s", depTag)
			}
		}
	}

	// 5. A subscription SOCKS node whose detour points at a tiered fallback
	// group retries the complete SOCKS CONNECT through another front.
	if upstream == nil && originalDetour != "" && b.detourMgr != nil {
		if opts, ok := outboundConfig.Options.(*option.SOCKSOutboundOptions); ok &&
			(opts.Version == "" || opts.Version == "5") {
			return &fallbackSOCKS{
				Outbound: ob, manager: b.detourMgr, detour: originalDetour, server: opts.ServerOptions.Build(),
				username: opts.Username, password: opts.Password,
			}, nil
		}
	}
	return ob, nil
}

// Close shuts down the builder's internal DNS services.
func (b *SingboxBuilder) Close() error {
	var errs []error
	if b.dnsRouter != nil {
		errs = append(errs, b.dnsRouter.Close())
	}
	if b.dnsTransportManager != nil {
		errs = append(errs, b.dnsTransportManager.Close())
	}
	return errors.Join(errs...)
}
