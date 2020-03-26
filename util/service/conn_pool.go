package rocserv

import (
	"context"
	"errors"
	xprom "gitlab.pri.ibanyu.com/middleware/seaweed/xstat/xmetric/xprometheus"
	"sync"
	"time"

	"gitlab.pri.ibanyu.com/middleware/seaweed/xutil"
	"gitlab.pri.ibanyu.com/middleware/seaweed/xutil/sync2"

	"github.com/shawnfeng/sutil/slog"
)

const (
	getConnTimeout = time.Second * 1
	statTick       = time.Second * 10
)

var (
	ErrConnectionPoolClosed = errors.New("connection pool is closed")
)

type rpcClientConn interface {
	Close()
	SetTimeout(timeout time.Duration) error
	GetServiceClient() interface{}
}

// ConnectionPool connection pool corresponding to the addr
type ConnectionPool struct {
	calleeServiceKey string

	rpcType     string
	rpcFactory  func(addr string) (rpcClientConn, error)
	addr        string
	capacity    int // capacity of pool
	maxCapacity int // max capacity of pool
	idleTimeout time.Duration

	mu          sync.Mutex
	closed      sync2.AtomicBool
	connections *xutil.ResourcePool // 对应地址的连接池
}

// NewConnectionPool constructor of ConnectionPool
func NewConnectionPool(addr string, capacity, maxCapacity int, idleTimeout time.Duration, rpcFactory func(addr string) (rpcClientConn, error), calleeServiceKey string) *ConnectionPool {
	cp := &ConnectionPool{addr: addr, capacity: capacity, maxCapacity: maxCapacity, idleTimeout: idleTimeout, rpcFactory: rpcFactory, calleeServiceKey: calleeServiceKey, closed: sync2.NewAtomicBool(false)}
	return cp
}

func (cp *ConnectionPool) Open() {
	if cp.capacity == 0 {
		cp.capacity = defaultMaxCapacity / 2
	}
	if cp.maxCapacity == 0 {
		cp.maxCapacity = defaultMaxCapacity
	}
	if cp.capacity > cp.maxCapacity {
		cp.maxCapacity = cp.capacity
	}
	if cp.maxCapacity < defaultMaxCapacity {
		cp.maxCapacity = defaultMaxCapacity
	}
	cp.mu.Lock()
	defer cp.mu.Unlock()
	cp.connections = xutil.NewResourcePool(cp.connect, cp.capacity, cp.maxCapacity, cp.idleTimeout)
	cp.stat()
	return
}

func (cp *ConnectionPool) stat() {
	tickC := time.Tick(statTick)
	go func() {
		for !cp.closed.Get() {
			select {
			case <-tickC:
				slog.Infof("caller: %s, callee: %s, callee_addr: %s, stat: %s", GetServName(), cp.calleeServiceKey, cp.addr, cp.connections.StatsJSON())
				group, service := GetGroupAndService()
				_metricRPCConnectionPool.With(xprom.LabelGroupName, group,
					xprom.LabelServiceName, service,
					xprom.LabelCalleeService, cp.calleeServiceKey,
					calleeAddr, cp.addr,
					connectionPoolStatType, active).Set(float64(cp.connections.Active()))
				_metricRPCConnectionPool.With(xprom.LabelGroupName, group,
					xprom.LabelServiceName, service,
					xprom.LabelCalleeService, cp.calleeServiceKey,
					calleeAddr, cp.addr,
					connectionPoolStatType, inUse).Set(float64(cp.connections.InUse()))
				_metricRPCConnectionPool.With(xprom.LabelGroupName, group,
					xprom.LabelServiceName, service,
					xprom.LabelCalleeService, cp.calleeServiceKey,
					calleeAddr, cp.addr,
					connectionPoolStatType, capacity).Set(float64(cp.connections.Capacity()))
				_metricRPCConnectionPool.With(xprom.LabelGroupName, group,
					xprom.LabelServiceName, service,
					xprom.LabelCalleeService, cp.calleeServiceKey,
					calleeAddr, cp.addr,
					connectionPoolStatType, maxCapacity).Set(float64(cp.connections.MaxCap()))
			}
		}
		slog.Infof("caller: %s, callee: %s, callee_addr: %s exit stat", GetServName(), cp.calleeServiceKey, cp.addr)
	}()
}

func (cp *ConnectionPool) pool() (p *xutil.ResourcePool) {
	cp.mu.Lock()
	p = cp.connections
	cp.mu.Unlock()
	return p
}

func (cp *ConnectionPool) connect() (xutil.Resource, error) {
	conn, err := cp.rpcFactory(cp.addr)
	return conn, err
}

// Close close connection pool
func (cp *ConnectionPool) Close() {
	cp.closed.Set(true)
	p := cp.pool()
	if p == nil {
		return
	}
	p.Close()
	cp.mu.Lock()
	cp.connections = nil
	cp.mu.Unlock()
	return
}

// Addr return addr of connection pool
func (cp *ConnectionPool) Addr() string {
	return cp.addr
}

// Get return a connection, you should call PooledConnection's Recycle once done
func (cp *ConnectionPool) Get(ctx context.Context) (rpcClientConn, error) {
	p := cp.pool()
	if p == nil {
		return nil, ErrConnectionPoolClosed
	}

	getCtx, cancel := context.WithTimeout(ctx, getConnTimeout)
	defer cancel()
	r, err := p.Get(getCtx)
	if err != nil {
		return nil, err
	}

	return r.(rpcClientConn), nil
}

// Put recycle a connection into the pool
func (cp *ConnectionPool) Put(conn rpcClientConn) {
	p := cp.pool()
	if p == nil {
		panic(ErrConnectionPoolClosed)
	}
	if conn == nil {
		p.Put(nil)
		return
	}
	p.Put(conn)
}

// SetCapacity alert the size of the pool at runtime
func (cp *ConnectionPool) SetCapacity(capacity int) (err error) {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	if cp.connections != nil {
		err = cp.connections.SetCapacity(capacity)
		if err != nil {
			return err
		}
	}
	cp.capacity = capacity
	return nil
}

// SetIdleTimeout set the idleTimeout of the pool
func (cp *ConnectionPool) SetIdleTimeout(idleTimeout time.Duration) {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	if cp.connections != nil {
		cp.connections.SetIdleTimeout(idleTimeout)
	}
	cp.idleTimeout = idleTimeout
}

// StatsJSON return the pool stats as JSON object.
func (cp *ConnectionPool) StatsJSON() string {
	p := cp.pool()
	if p == nil {
		return "{}"
	}
	return p.StatsJSON()
}

// Capacity return the pool capacity
func (cp *ConnectionPool) Capacity() int64 {
	p := cp.pool()
	if p == nil {
		return 0
	}
	return p.Capacity()
}

// Available returns the number of available connections in the pool
func (cp *ConnectionPool) Available() int64 {
	p := cp.pool()
	if p == nil {
		return 0
	}
	return p.Available()
}

// Active returns the number of active connections in the pool
func (cp *ConnectionPool) Active() int64 {
	p := cp.pool()
	if p == nil {
		return 0
	}
	return p.Active()
}

// InUse returns the number of in-use connections in the pool
func (cp *ConnectionPool) InUse() int64 {
	p := cp.pool()
	if p == nil {
		return 0
	}
	return p.InUse()
}

// MaxCap returns the maximum size of the pool
func (cp *ConnectionPool) MaxCap() int64 {
	p := cp.pool()
	if p == nil {
		return 0
	}
	return p.MaxCap()
}

// WaitCount returns how many clients are waitting for a connection
func (cp *ConnectionPool) WaitCount() int64 {
	p := cp.pool()
	if p == nil {
		return 0
	}
	return p.WaitCount()
}

// WaitTime returns the time wait for a connection
func (cp *ConnectionPool) WaitTime() time.Duration {
	p := cp.pool()
	if p == nil {
		return 0
	}
	return p.WaitTime()
}

// IdleTimeout returns the idle timeout for the pool
func (cp *ConnectionPool) IdleTimeout() time.Duration {
	p := cp.pool()
	if p == nil {
		return 0
	}
	return p.IdleTimeout()
}

// IdleClosed return the number of closed connections for the pool
func (cp *ConnectionPool) IdleClosed() int64 {
	p := cp.pool()
	if p == nil {
		return 0
	}
	return p.IdleClosed()
}
