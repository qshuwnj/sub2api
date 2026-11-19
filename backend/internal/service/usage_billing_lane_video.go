package service

import (
	"context"
	"errors"
)

var (
	_ VideoChargeReceiptReader = (*UsageBillingLaneCombiner)(nil)
	_ VideoTaskStateRepository = (*UsageBillingLaneCombiner)(nil)
)

// Reads and task snapshots bypass the batching queue; only Apply combines debits.
func (c *UsageBillingLaneCombiner) FindVideoChargeReceipt(ctx context.Context, owner VideoAttemptOwner, task string) (*VideoChargeReceipt, error) {
	if c == nil {
		return nil, errors.New("video charge receipt reader is unavailable")
	}
	reader, ok := c.repo.(VideoChargeReceiptReader)
	if !ok {
		return nil, errors.New("usage billing repository does not support video charge receipts")
	}
	return reader.FindVideoChargeReceipt(ctx, owner, task)
}

func (c *UsageBillingLaneCombiner) videoTaskStore() (VideoTaskStateRepository, error) {
	if c == nil {
		return nil, errors.New("video task storage is unavailable")
	}
	repo, ok := c.repo.(VideoTaskStateRepository)
	if !ok {
		return nil, errors.New("usage billing repository does not support video task storage")
	}
	return repo, nil
}

func (c *UsageBillingLaneCombiner) CheckVideoTaskStorage(ctx context.Context) error {
	repo, err := c.videoTaskStore()
	if err != nil {
		return err
	}
	return repo.CheckVideoTaskStorage(ctx)
}

func (c *UsageBillingLaneCombiner) SaveVideoTaskState(ctx context.Context, state VideoTaskState) error {
	repo, err := c.videoTaskStore()
	if err != nil {
		return err
	}
	return repo.SaveVideoTaskState(ctx, state)
}

func (c *UsageBillingLaneCombiner) LoadVideoTaskState(ctx context.Context, task string, userID, apiKeyID int64) (*VideoTaskState, error) {
	repo, err := c.videoTaskStore()
	if err != nil {
		return nil, err
	}
	return repo.LoadVideoTaskState(ctx, task, userID, apiKeyID)
}

func (c *UsageBillingLaneCombiner) UpdateVideoTaskBilling(ctx context.Context, task string, userID, apiKeyID int64, pending GrokVideoPendingBilling) error {
	repo, err := c.videoTaskStore()
	if err != nil {
		return err
	}
	return repo.UpdateVideoTaskBilling(ctx, task, userID, apiKeyID, pending)
}
