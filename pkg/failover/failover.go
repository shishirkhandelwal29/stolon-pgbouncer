//go:generate protoc --go_out=plugins=grpc:. failover.proto

package failover

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/gocardless/stolon-pgbouncer/pkg/etcd"
	"github.com/gocardless/stolon-pgbouncer/pkg/stolon"
	"github.com/gocardless/stolon-pgbouncer/pkg/streams"

	"github.com/coreos/etcd/clientv3"
	"github.com/coreos/etcd/clientv3/concurrency"
	kitlog "github.com/go-kit/kit/log"
	"github.com/pkg/errors"
)

type Failover struct {
	logger    kitlog.Logger
	client    *clientv3.Client
	clients   map[string]FailoverClient
	stolonctl stolon.Stolonctl
	locker    locker
	opt       FailoverOptions
}

type FailoverOptions struct {
	ClusterdataKey     string
	HealthCheckTimeout time.Duration
	LockTimeout        time.Duration
	PauseTimeout       time.Duration
	PauseExpiry        time.Duration
	ResumeTimeout      time.Duration
	StolonctlTimeout   time.Duration
}

type locker interface {
	Lock(context.Context) error
	Unlock(context.Context) error
}

func NewFailover(logger kitlog.Logger, client *clientv3.Client, clients map[string]FailoverClient, stolonctl stolon.Stolonctl, opt FailoverOptions) *Failover {
	session, _ := concurrency.NewSession(client)

	return &Failover{
		logger:    logger,
		client:    client,
		clients:   clients,
		stolonctl: stolonctl,
		opt:       opt,
		locker: concurrency.NewMutex(
			session, fmt.Sprintf("%s/failover", opt.ClusterdataKey),
		),
	}
}

// Run triggers the failover process. We model this as a Pipeline of steps, where each
// step has associated deferred actions that must be scheduled before the primary
// operation ever takes place.
//
// This has the benefit of clearly expressing the steps required to perform a failover,
// tidying up some of the error handling and logging noise that would otherwise be
// present.
func (f *Failover) Run(ctx context.Context, deferCtx context.Context) error {
	return Pipeline(
		Step(f.HealthCheckClients),
		Step(f.AcquireLock).Defer(f.ReleaseLock),
		Step(f.Pause).Defer(f.Resume),
		Step(f.Failkeeper),
	)(
		ctx, deferCtx,
	)
}

func (f *Failover) HealthCheckClients(ctx context.Context) error {
	f.logger.Log("event", "clients.health_check", "msg", "health checking all clients")
	for endpoint, client := range f.clients {
		ctx, cancel := context.WithTimeout(ctx, f.opt.HealthCheckTimeout)
		defer cancel()

		resp, err := client.HealthCheck(ctx, &Empty{})
		if err != nil {
			return errors.Wrapf(err, "client %s failed health check", endpoint)
		}

		if status := resp.GetStatus(); status != HealthCheckResponse_HEALTHY {
			return fmt.Errorf("client %s received non-healthy response: %s", endpoint, status.String())
		}
	}

	return nil
}

func (f *Failover) AcquireLock(ctx context.Context) error {
	f.logger.Log("event", "etcd.lock.acquire", "msg", "acquiring failover lock in etcd")
	ctx, cancel := context.WithTimeout(ctx, f.opt.LockTimeout)
	defer cancel()

	return f.locker.Lock(ctx)
}

func (f *Failover) ReleaseLock(ctx context.Context) error {
	f.logger.Log("event", "etcd.lock.release", "msg", "releasing failover lock in etcd")
	ctx, cancel := context.WithTimeout(ctx, f.opt.LockTimeout)
	defer cancel()

	return f.locker.Unlock(ctx)
}

func (f *Failover) Pause(ctx context.Context) error {
	logger := kitlog.With(f.logger, "event", "clients.pgbouncer.pause")
	logger.Log("msg", "requesting all pgbouncers pause")

	// Allow an additional second for network round-trip. We should have terminated this
	// request far before this context is expired.
	ctx, cancel := context.WithTimeout(ctx, f.opt.PauseExpiry+time.Second)
	defer cancel()

	err := f.EachClient(logger, func(endpoint string, client FailoverClient) error {
		_, err := client.Pause(
			ctx, &PauseRequest{
				Timeout: int64(f.opt.PauseTimeout),
				Expiry:  int64(f.opt.PauseExpiry),
			},
		)

		return err
	})

	if err != nil {
		return fmt.Errorf("failed to pause pgbouncers")
	}

	return nil
}

func (f *Failover) Resume(ctx context.Context) error {
	logger := kitlog.With(f.logger, "event", "clients.pgbouncer.resume")
	logger.Log("msg", "requesting all pgbouncers resume")

	ctx, cancel := context.WithTimeout(ctx, f.opt.ResumeTimeout)
	defer cancel()

	err := f.EachClient(logger, func(endpoint string, client FailoverClient) error {
		_, err := client.Resume(ctx, &Empty{})
		return err
	})

	if err != nil {
		return fmt.Errorf("failed to resume pgbouncers")
	}

	return nil
}

// EachClient provides a helper to perform actions on all the failover clients, in
// parallel. For some operations where there is a penalty for extended running time (such
// as pause) it's important that each request occurs in parallel.
func (f *Failover) EachClient(logger kitlog.Logger, action func(string, FailoverClient) error) (result error) {
	var wg sync.WaitGroup
	for endpoint, client := range f.clients {
		wg.Add(1)

		go func(endpoint string, client FailoverClient) {
			defer func(begin time.Time) {
				logger.Log("endpoint", endpoint, "elapsed", time.Since(begin).Seconds())
				wg.Done()
			}(time.Now())

			if err := action(endpoint, client); err != nil {
				logger.Log("endpoint", endpoint, "error", err.Error())
				result = err
			}
		}(endpoint, client)
	}

	wg.Wait()
	return result
}

// Failkeeper uses stolonctl to mark the current primary keeper as failed
func (f *Failover) Failkeeper(ctx context.Context) error {
	clusterdata, err := stolon.GetClusterdata(ctx, f.client, f.opt.ClusterdataKey)
	if err != nil {
		return err
	}

	master := clusterdata.Master()
	masterKeeperUID := master.Spec.KeeperUID
	if masterKeeperUID == "" {
		return errors.New("could not identify master keeper")
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, f.opt.StolonctlTimeout)
	defer cancel()

	cmd := f.stolonctl.CommandContext(timeoutCtx, "failkeeper", masterKeeperUID)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return errors.Wrap(err, "failed to run stolonctl failkeeper")
	}

	select {
	case <-time.After(f.opt.PauseExpiry):
		return fmt.Errorf("timed out waiting for successful recovery")
	case newMaster := <-f.NotifyRecovered(ctx, f.logger, master):
		f.logger.Log("msg", "cluster successfully recovered", "master", newMaster)
	}

	return nil
}

// NotifyRecovered will return a channel that receives the new master DB only once it is
// healthy and available for writes. We determine this by checking the new master and all
// its sync nodes are healthy.
func (f *Failover) NotifyRecovered(ctx context.Context, logger kitlog.Logger, oldMaster stolon.DB) chan stolon.DB {
	logger = kitlog.With(logger, "key", f.opt.ClusterdataKey)
	logger.Log("msg", "waiting for stolon to report master change")

	kvs, _ := etcd.NewStream(
		f.logger,
		f.client,
		etcd.StreamOptions{
			Ctx:          ctx,
			Keys:         []string{f.opt.ClusterdataKey},
			PollInterval: time.Second,
			GetTimeout:   time.Second,
		},
	)

	kvs = streams.RevisionFilter(f.logger, kvs)

	notify := make(chan stolon.DB)
	go func() {
		for kv := range kvs {
			if string(kv.Key) != f.opt.ClusterdataKey {
				continue
			}

			var clusterdata = &stolon.Clusterdata{}
			if err := json.Unmarshal(kv.Value, clusterdata); err != nil {
				logger.Log("error", err, "msg", "failed to parse clusterdata update")
				continue
			}

			master := clusterdata.Master()
			if master.Spec.KeeperUID == oldMaster.Spec.KeeperUID {
				logger.Log("event", "pending_failover", "master", master, "msg", "master has not changed nodes")
				continue
			}

			if !master.Status.Healthy {
				logger.Log("event", "master.unhealthy", "master", master, "msg", "new master is unhealthy")
				continue
			}

			anyUnhealthyStandbys := false
			for _, standby := range clusterdata.SynchronousStandbys() {
				if !standby.Status.Healthy {
					logger.Log("event", "standby.unhealthy", "standby", standby)
					anyUnhealthyStandbys = true
				}
			}

			if anyUnhealthyStandbys {
				continue
			}

			logger.Log("event", "healthy", "master", master, "msg", "master is available for writes")
		}
	}()

	return notify
}