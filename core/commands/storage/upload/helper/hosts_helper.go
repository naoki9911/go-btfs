package helper

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/TRON-US/go-btfs/core/commands/storage/helper"
	hubpb "github.com/tron-us/go-btfs-common/protos/hub"

	"github.com/libp2p/go-libp2p-core/peer"
)

const (
	minimumHosts = 30
	failMsg      = "failed to find more valid hosts, please try again later"
	parallelism  = 10
)

type IHostsProvider interface {
	NextValidHost(price int64) (string, error)
}

type CustomizedHostsProvider struct {
	cp      *ContextParams
	current int
	hosts   []string
	sync.Mutex
}

func (p *CustomizedHostsProvider) NextValidHost(price int64) (string, error) {
	for true {
		index, err := p.AddIndex()
		fmt.Println("index", index, "err", err)
		if err == nil {
			id, err := peer.IDB58Decode(p.hosts[index])
			if err != nil {
				fmt.Printf("decode err", err)
				continue
			}
			fmt.Println("index", index, "id", id)
			if err := p.cp.Api.Swarm().Connect(p.cp.Ctx, peer.AddrInfo{ID: id}); err != nil {
				fmt.Println("index", index, "id", id, "err", err)
				continue
			}
			fmt.Println("index", index, "id", id, "err2", err)
			return p.hosts[index], nil
		} else {
			break
		}
	}
	return "", errors.New(failMsg)
}

func GetCustomizedHostsProvider(cp *ContextParams, hosts []string) IHostsProvider {
	return &CustomizedHostsProvider{
		cp:      cp,
		current: -1,
		hosts:   append(hosts, hosts...),
	}
}

func (p *CustomizedHostsProvider) AddIndex() (int, error) {
	p.Lock()
	defer p.Unlock()
	p.current++
	fmt.Println("current", p.current, "len(hosts)", len(p.hosts))
	if p.current >= len(p.hosts) {
		return -1, errors.New("Index exceeds array bounds.")
	}
	return p.current, nil
}

type HostsProvider struct {
	cp *ContextParams
	sync.Mutex
	mode      string
	current   int
	hosts     []*hubpb.Host
	blacklist []string
	ctx       context.Context
	cancel    context.CancelFunc
}

func GetHostsProvider(cp *ContextParams, blacklist []string) IHostsProvider {
	ctx, cancel := context.WithTimeout(cp.Ctx, 10*time.Minute)
	p := &HostsProvider{
		cp:        cp,
		mode:      cp.Cfg.Experimental.HostsSyncMode,
		current:   -1,
		blacklist: blacklist,
		ctx:       ctx,
		cancel:    cancel,
	}
	p.init()
	return p
}

func (p *HostsProvider) init() (err error) {
	p.hosts, err = helper.GetHostsFromDatastore(p.cp.Ctx, p.cp.N, p.mode, minimumHosts)
	return err
}

func (p *HostsProvider) AddIndex() (int, error) {
	p.Lock()
	defer p.Unlock()
	p.current++
	if p.current >= len(p.hosts) {
		return -1, errors.New("Index exceeds array bounds.")
	}
	return p.current, nil
}

func (p *HostsProvider) NextValidHost(price int64) (string, error) {
	needHigherPrice := false
LOOP:
	for true {
		select {
		case <-p.ctx.Done():
			p.cancel()
			return "", errors.New(failMsg)
		default:
		}
		if index, err := p.AddIndex(); err == nil {
			host := p.hosts[index]
			for _, h := range p.blacklist {
				if h == host.NodeId {
					continue LOOP
				}
			}
			id, err := peer.IDB58Decode(host.NodeId)
			if err != nil || int64(host.StoragePriceAsk) > price {
				needHigherPrice = true
				continue
			}
			ctx, _ := context.WithTimeout(p.ctx, 7*time.Second)

			errChan := make(chan error)
			for i := 0; i < parallelism; i++ {
				go func() {
					errChan <- p.cp.Api.Swarm().Connect(ctx, peer.AddrInfo{ID: id})
				}()
			}
			tryCount := 0
			for err := range errChan {
				fmt.Println("index", index, "id", id, "err", err)
				tryCount++
				if err == nil {
					break
				}
				if tryCount == parallelism {
					p.Lock()
					p.hosts = append(p.hosts, host)
					p.Unlock()
					continue LOOP
				}
			}
			return host.NodeId, nil
		} else {
			break
		}
	}
	msg := failMsg
	if needHigherPrice {
		msg += " or raise price"
	}
	return "", errors.New(msg)
}
