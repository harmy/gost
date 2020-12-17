package gost

import (
	"context"
	"errors"
	"math/rand"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Dreamacro/clash/adapters/outbound"
	"github.com/go-log/log"
)

var (
	// ErrNoneAvailable indicates there is no node available.
	ErrNoneAvailable = errors.New("none available")
)

// NodeSelector as a mechanism to pick nodes and mark their status.
type NodeSelector interface {
	Select(nodes []Node, opts ...SelectOption) (Node, error)
}

type defaultSelector struct {
}

func (s *defaultSelector) Select(nodes []Node, opts ...SelectOption) (Node, error) {
	sopts := SelectOptions{}
	for _, opt := range opts {
		opt(&sopts)
	}

	for _, filter := range sopts.Filters {
		nodes = filter.Filter(nodes)
	}
	if len(nodes) == 0 {
		return Node{}, ErrNoneAvailable
	}
	strategy := sopts.Strategy
	if strategy == nil {
		strategy = &RoundStrategy{}
	}
	return strategy.Apply(nodes), nil
}

// SelectOption is the option used when making a select call.
type SelectOption func(*SelectOptions)

// SelectOptions is the options for node selection.
type SelectOptions struct {
	Filters  []Filter
	Strategy Strategy
}

// WithFilter adds a filter function to the list of filters
// used during the Select call.
func WithFilter(f ...Filter) SelectOption {
	return func(o *SelectOptions) {
		o.Filters = append(o.Filters, f...)
	}
}

// WithStrategy sets the selector strategy
func WithStrategy(s Strategy) SelectOption {
	return func(o *SelectOptions) {
		o.Strategy = s
	}
}

// Strategy is a selection strategy e.g random, round-robin.
type Strategy interface {
	Apply([]Node) Node
	String() string
}

// NewStrategy creates a Strategy by the name s.
func NewStrategy(s string) Strategy {
	switch s {
	case "random":
		return &RandomStrategy{}
	case "fifo":
		return &FIFOStrategy{}
	case "round":
		fallthrough
	default:
		return &RoundStrategy{}
	}
}

// RoundStrategy is a strategy for node selector.
// The node will be selected by round-robin algorithm.
type RoundStrategy struct {
	counter uint64
}

// Apply applies the round-robin strategy for the nodes.
func (s *RoundStrategy) Apply(nodes []Node) Node {
	if len(nodes) == 0 {
		return Node{}
	}

	n := atomic.AddUint64(&s.counter, 1) - 1
	return nodes[int(n%uint64(len(nodes)))]
}

func (s *RoundStrategy) String() string {
	return "round"
}

// RandomStrategy is a strategy for node selector.
// The node will be selected randomly.
type RandomStrategy struct {
	Seed int64
	rand *rand.Rand
	once sync.Once
	mux  sync.Mutex
}

// Apply applies the random strategy for the nodes.
func (s *RandomStrategy) Apply(nodes []Node) Node {
	s.once.Do(func() {
		seed := s.Seed
		if seed == 0 {
			seed = time.Now().UnixNano()
		}
		s.rand = rand.New(rand.NewSource(seed))
	})
	if len(nodes) == 0 {
		return Node{}
	}

	s.mux.Lock()
	r := s.rand.Int()
	s.mux.Unlock()

	return nodes[r%len(nodes)]
}

func (s *RandomStrategy) String() string {
	return "random"
}

// FIFOStrategy is a strategy for node selector.
// The node will be selected from first to last,
// and will stick to the selected node until it is failed.
type FIFOStrategy struct{}

// Apply applies the fifo strategy for the nodes.
func (s *FIFOStrategy) Apply(nodes []Node) Node {
	if len(nodes) == 0 {
		return Node{}
	}
	return nodes[0]
}

func (s *FIFOStrategy) String() string {
	return "fifo"
}

// Filter is used to filter a node during the selection process
type Filter interface {
	Filter([]Node) []Node
	String() string
}

// default options for FailFilter
const (
	DefaultMaxFails    = 1
	DefaultFailTimeout = 30 * time.Second
)

// FailFilter filters the dead node.
// A node is marked as dead if its failed count is greater than MaxFails.
type FailFilter struct {
	MaxFails    int
	FailTimeout time.Duration
}

// Filter filters dead nodes.
func (f *FailFilter) Filter(nodes []Node) []Node {
	maxFails := f.MaxFails
	if maxFails == 0 {
		maxFails = DefaultMaxFails
	}
	failTimeout := f.FailTimeout
	if failTimeout == 0 {
		failTimeout = DefaultFailTimeout
	}

	if len(nodes) <= 1 || maxFails < 0 {
		return nodes
	}
	nl := []Node{}
	for _, node := range nodes {
		marker := node.marker.Clone()
		if Debug {
			log.Logf("[fail filter] %d@%s: %d/%d %v/%v", node.ID, node, marker.FailCount(), f.MaxFails, marker.FailTime(), f.FailTimeout)
		}
		if marker.FailCount() < uint32(maxFails) ||
			time.Since(time.Unix(marker.FailTime(), 0)) >= failTimeout {
			nl = append(nl, node)
		}
	}
	if Debug {
		log.Logf("[fail filter] %v", nl)
	}
	return nl
}

func (f *FailFilter) String() string {
	return "fail"
}

// InvalidFilter filters the invalid node.
// A node is invalid if its port is invalid (negative or zero value).
type InvalidFilter struct{}

// Filter filters invalid nodes.
func (f *InvalidFilter) Filter(nodes []Node) []Node {
	nl := []Node{}
	for _, node := range nodes {
		_, sport, _ := net.SplitHostPort(node.Addr)
		if port, _ := strconv.Atoi(sport); port > 0 {
			nl = append(nl, node)
		}
	}
	return nl
}

func (f *InvalidFilter) String() string {
	return "invalid"
}

// DeadFilter filters the dead node.
// A node is dead if its delay is 65535.
type DeadFilter struct{}

// Filter filters dead nodes.
func (f *DeadFilter) Filter(nodes []Node) []Node {
	nl := []Node{}
	for _, node := range nodes {
		tester := node.tester.Clone()
		if Debug {
			log.Logf("[dead filter] %d@%s: %d/%v", node.ID, node, tester.Proxy.LastDelay(), tester.Proxy.Alive())
		}
		if tester.Proxy.Alive() {
			nl = append(nl, node)
		}
	}
	if Debug {
		log.Logf("[dead filter] %v", nl)
	}
	return nl
}

func (f *DeadFilter) String() string {
	return "dead"
}

type failMarker struct {
	failTime  int64
	failCount uint32
	mux       sync.RWMutex
}

func (m *failMarker) FailTime() int64 {
	if m == nil {
		return 0
	}

	m.mux.Lock()
	defer m.mux.Unlock()

	return m.failTime
}

func (m *failMarker) FailCount() uint32 {
	if m == nil {
		return 0
	}

	m.mux.Lock()
	defer m.mux.Unlock()

	return m.failCount
}

func (m *failMarker) Mark() {
	if m == nil {
		return
	}

	m.mux.Lock()
	defer m.mux.Unlock()

	m.failTime = time.Now().Unix()
	m.failCount++
}

func (m *failMarker) Reset() {
	if m == nil {
		return
	}

	m.mux.Lock()
	defer m.mux.Unlock()

	m.failTime = 0
	m.failCount = 0
}

func (m *failMarker) Clone() *failMarker {
	if m == nil {
		return nil
	}

	m.mux.RLock()
	defer m.mux.RUnlock()

	fc, ft := m.failCount, m.failTime

	return &failMarker{
		failCount: fc,
		failTime:  ft,
	}
}

type delayTester struct {
	Proxy *outbound.Proxy
	mux   sync.RWMutex
}

func (m *delayTester) LastTestTime() (res int64) {
	if m == nil || m.Proxy == nil {
		return 0
	}

	m.mux.Lock()
	defer m.mux.Unlock()
	history := m.Proxy.DelayHistory()
	if len(history) > 0 {
		res = history[len(history)-1].Time.Unix()
	}
	return
}

func (m *delayTester) LastDelay() uint16 {
	if m == nil || m.Proxy == nil {
		return 0
	}

	m.mux.Lock()
	defer m.mux.Unlock()

	return m.Proxy.LastDelay()
}

func (m *delayTester) TestDelay(node *Node) (err error) {
	if m == nil || m.Proxy == nil {
		return
	}

	m.mux.Lock()
	defer m.mux.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()
	urls := []string{
		"http://www.gstatic.com/generate_204",
		"http://www.msftconnecttest.com/connecttest.txt",
		"http://captive.apple.com",
		"http://detectportal.firefox.com/success.txt",
		"http://cp.cloudflare.com",
	}
	_, err = m.Proxy.URLTest(ctx, urls[rand.Intn(len(urls))])
	if Debug {
		log.Logf("[tester] delay: %d", m.Proxy.LastDelay())
	}
	return
}

func (m *delayTester) Clone() *delayTester {
	if m == nil {
		return nil
	}

	m.mux.RLock()
	defer m.mux.RUnlock()

	return &delayTester{
		Proxy: m.Proxy,
	}
}
