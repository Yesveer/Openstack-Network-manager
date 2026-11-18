package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"syscall"
	"time"

	"github.com/vishvananda/netlink"
	"github.com/yesveer/openstack-net-attach/internal/network"
)

// Daemon monitors netlink events and configures network interfaces as they are
// hot-plugged or hot-unplugged in an OpenStack virtual machine.
type Daemon struct {
	manager *network.Manager
	logger  *slog.Logger
}

func New(manager *network.Manager, logger *slog.Logger) *Daemon {
	return &Daemon{
		manager: manager,
		logger:  logger,
	}
}

// Run starts the daemon. It blocks until ctx is cancelled or a fatal netlink error occurs.
// On transient netlink errors the subscription is re-established automatically.
func (d *Daemon) Run(ctx context.Context) error {
	d.logger.Info("scanning existing interfaces")
	d.manager.ConfigureExisting(ctx)

	for {
		if ctx.Err() != nil {
			return nil
		}

		err := d.runLoop(ctx)
		if ctx.Err() != nil {
			return nil
		}

		d.logger.Error("netlink subscription failed, restarting in 5s", "error", err)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(5 * time.Second):
		}
	}
}

// runLoop subscribes to netlink and processes events until an error or ctx cancellation.
func (d *Daemon) runLoop(ctx context.Context) error {
	updates := make(chan netlink.LinkUpdate, 64)
	done := make(chan struct{})
	defer close(done)

	errCh := make(chan error, 1)
	opts := netlink.LinkSubscribeOptions{
		ErrorCallback: func(err error) {
			select {
			case errCh <- err:
			default:
			}
		},
	}

	if err := netlink.LinkSubscribeWithOptions(updates, done, opts); err != nil {
		return fmt.Errorf("subscribing to netlink: %w", err)
	}

	d.logger.Info("monitoring network interfaces for hot-plug events")

	for {
		select {
		case <-ctx.Done():
			return nil

		case err := <-errCh:
			return fmt.Errorf("netlink error: %w", err)

		case update, ok := <-updates:
			if !ok {
				return fmt.Errorf("netlink channel closed unexpectedly")
			}
			d.handleUpdate(ctx, update)
		}
	}
}

func (d *Daemon) handleUpdate(ctx context.Context, update netlink.LinkUpdate) {
	name := update.Link.Attrs().Name

	if !d.manager.ShouldManage(name) {
		return
	}

	switch update.Header.Type {
	case syscall.RTM_NEWLINK:
		d.logger.Debug("RTM_NEWLINK received", "interface", name)
		// Fire-and-forget; ConfigureInterface deduplicates concurrent calls.
		go d.manager.ConfigureInterface(ctx, name)

	case syscall.RTM_DELLINK:
		d.logger.Info("interface hot-unplugged", "interface", name)
		go d.manager.ReleaseInterface(ctx, name, update.Link.Attrs().HardwareAddr)
	}
}
