package db

import (
	"errors"
	"fmt"

	"github.com/juanfont/headscale/hscontrol/types"
	"gorm.io/gorm"
)

var ErrTailnetNotFound = errors.New("tailnet not found")

// ListTailnets returns all tailnets from the database.
func (hsdb *HSDatabase) ListTailnets() ([]types.Tailnet, error) {
	return Read(hsdb.DB, func(rx *gorm.DB) ([]types.Tailnet, error) {
		return listTailnets(rx)
	})
}

func listTailnets(tx *gorm.DB) ([]types.Tailnet, error) {
	var tailnets []types.Tailnet

	err := tx.Find(&tailnets).Error
	if err != nil {
		return nil, fmt.Errorf("listing tailnets: %w", err)
	}

	return tailnets, nil
}

// GetTailnetByID returns a tailnet by its ID.
func (hsdb *HSDatabase) GetTailnetByID(id uint) (*types.Tailnet, error) {
	return Read(hsdb.DB, func(rx *gorm.DB) (*types.Tailnet, error) {
		return getTailnetByID(rx, id)
	})
}

func getTailnetByID(tx *gorm.DB, id uint) (*types.Tailnet, error) {
	var tailnet types.Tailnet

	err := tx.First(&tailnet, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrTailnetNotFound
	}

	if err != nil {
		return nil, fmt.Errorf("getting tailnet %d: %w", id, err)
	}

	return &tailnet, nil
}

// GetTailnetByName returns a tailnet by its name.
func (hsdb *HSDatabase) GetTailnetByName(name string) (*types.Tailnet, error) {
	return Read(hsdb.DB, func(rx *gorm.DB) (*types.Tailnet, error) {
		var tailnet types.Tailnet

		err := rx.Where("name = ?", name).First(&tailnet).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrTailnetNotFound
		}

		if err != nil {
			return nil, fmt.Errorf("getting tailnet %q: %w", name, err)
		}

		return &tailnet, nil
	})
}

// CreateTailnet creates a new tailnet in the database.
func (hsdb *HSDatabase) CreateTailnet(tailnet *types.Tailnet) error {
	return hsdb.Write(func(tx *gorm.DB) error {
		return tx.Create(tailnet).Error
	})
}
