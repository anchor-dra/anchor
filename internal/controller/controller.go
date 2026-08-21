package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	anchormetrics "github.com/anchor-dra/anchor/internal/metrics"
)

type Controller struct {
	Inventory         *InventoryReconciler
	Placement         *PlacementReconciler
	Interval          time.Duration
	InventoryInterval time.Duration
	DriftInterval     time.Duration
	ReadinessInterval time.Duration
	Logger            *slog.Logger
}

func (c *Controller) Run(ctx context.Context) error {
	if c.Inventory == nil || c.Placement == nil {
		return fmt.Errorf("inventory and placement reconcilers are required")
	}
	if c.Interval <= 0 {
		c.Interval = 2 * time.Second
	}
	if c.InventoryInterval <= 0 {
		c.InventoryInterval = time.Minute
	}
	if c.DriftInterval <= 0 {
		c.DriftInterval = time.Minute
	}
	if c.ReadinessInterval <= 0 {
		c.ReadinessInterval = 5 * time.Second
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	placementTicker := time.NewTicker(c.Interval)
	inventoryTicker := time.NewTicker(c.InventoryInterval)
	driftTicker := time.NewTicker(c.DriftInterval)
	readinessTicker := time.NewTicker(c.ReadinessInterval)
	defer placementTicker.Stop()
	defer inventoryTicker.Stop()
	defer driftTicker.Stop()
	defer readinessTicker.Stop()
	c.runAndLog(ctx, "placement", c.reconcilePlacement)
	c.runAndLog(ctx, "inventory", c.reconcileInventory)
	c.runAndLog(ctx, "readiness", c.reconcileReadiness)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-placementTicker.C:
			c.runAndLog(ctx, "placement", c.reconcilePlacement)
		case <-inventoryTicker.C:
			c.runAndLog(ctx, "inventory", c.reconcileInventory)
		case <-driftTicker.C:
			c.runAndLog(ctx, "placement", c.reconcilePlacement)
			c.runAndLog(ctx, "drift", c.reconcileDrift)
		case <-readinessTicker.C:
			c.runAndLog(ctx, "readiness", c.reconcileReadiness)
		}
	}
}

func (c *Controller) Reconcile(ctx context.Context) error {
	if c.Inventory == nil || c.Placement == nil {
		return fmt.Errorf("inventory and placement reconcilers are required")
	}
	placementErr := c.reconcilePlacement(ctx)
	inventoryErr := c.reconcileInventory(ctx)
	readinessErr := c.reconcileReadiness(ctx)
	return errors.Join(inventoryErr, placementErr, readinessErr)
}

func (c *Controller) reconcileInventory(ctx context.Context) error {
	return c.observe("inventory", func() error { return c.Inventory.Reconcile(ctx) })
}

func (c *Controller) reconcilePlacement(ctx context.Context) error {
	return c.observe("placement", func() error { return c.Placement.Reconcile(ctx) })
}

func (c *Controller) reconcileDrift(ctx context.Context) error {
	return c.observe("drift", func() error { return c.Placement.VerifyReady(ctx) })
}

func (c *Controller) reconcileReadiness(ctx context.Context) error {
	return c.observe("readiness", func() error { return c.Inventory.ReconcileTaints(ctx) })
}

func (c *Controller) observe(name string, reconcile func() error) error {
	started := time.Now()
	defer func() { anchormetrics.ReconcileDuration.WithLabelValues(name).Observe(time.Since(started).Seconds()) }()
	err := reconcile()
	if err != nil {
		anchormetrics.ReconcileErrors.WithLabelValues(name).Inc()
	}
	return err
}

func (c *Controller) runAndLog(ctx context.Context, name string, reconcile func(context.Context) error) {
	if err := reconcile(ctx); err != nil {
		c.Logger.Error("controller reconciliation failed", "controller", name, "error", err)
	}
}
