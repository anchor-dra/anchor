package controller

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	anchormetrics "example.com/anchor/internal/metrics"
)

type Controller struct {
	Inventory *InventoryReconciler
	Placement *PlacementReconciler
	Interval  time.Duration
	Logger    *slog.Logger
}

func (c *Controller) Run(ctx context.Context) error {
	if c.Interval <= 0 {
		c.Interval = 2 * time.Second
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	ticker := time.NewTicker(c.Interval)
	defer ticker.Stop()
	for {
		if err := c.Reconcile(ctx); err != nil {
			c.Logger.Error("controller reconciliation failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (c *Controller) Reconcile(ctx context.Context) error {
	if c.Inventory == nil || c.Placement == nil {
		return fmt.Errorf("inventory and placement reconcilers are required")
	}
	if err := c.Inventory.Reconcile(ctx); err != nil {
		anchormetrics.ReconcileErrors.WithLabelValues("inventory").Inc()
		return err
	}
	if err := c.Placement.Reconcile(ctx); err != nil {
		anchormetrics.ReconcileErrors.WithLabelValues("placement").Inc()
		return err
	}
	return nil
}
