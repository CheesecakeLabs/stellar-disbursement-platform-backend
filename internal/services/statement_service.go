package services

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stellar/go-stellar-sdk/clients/horizonclient"
	"github.com/stellar/go-stellar-sdk/protocols/horizon"
	"github.com/stellar/go-stellar-sdk/protocols/horizon/base"
	"github.com/stellar/go-stellar-sdk/protocols/horizon/operations"

	"github.com/stellar/stellar-disbursement-platform-backend/internal/data"
	"github.com/stellar/stellar-disbursement-platform-backend/internal/services/assets"
	"github.com/stellar/stellar-disbursement-platform-backend/pkg/schema"
)

const (
	// MaxStatementTransactionsPages caps pagination over Horizon transactions to avoid unbounded loops.
	MaxStatementTransactionsPages = 50
	// StatementTransactionsPageLimit is the page size when fetching transactions from Horizon.
	StatementTransactionsPageLimit = 200
)

var (
	ErrStatementAccountNotStellar = errors.New("statement is only supported for Stellar distribution accounts")
	ErrStatementAssetNotFound     = errors.New("asset not found for account")
)

// StatementServiceInterface defines the interface for generating account statements.
type StatementServiceInterface interface {
	GetStatement(ctx context.Context, account *schema.TransactionAccount, assetCode string, fromDate, toDate time.Time) (*StatementResult, error)
}

// StatementResult is the full statement response.
type StatementResult struct {
	Summary      StatementSummary       `json:"summary"`
	Transactions []StatementTransaction `json:"transactions"`
	Totals       StatementTotals        `json:"totals"`
}

// StatementSummary holds the statement summary section.
type StatementSummary struct {
	Account            string   `json:"account"`
	Asset              AssetRef `json:"asset"`
	BeginningBalance   string   `json:"beginning_balance"`
	TotalCredits       string   `json:"total_credits"`
	TotalDebits        string   `json:"total_debits"`
	EndingBalance      string   `json:"ending_balance"`
	InvolvedWalletIDs  []string `json:"involved_wallet_ids"`
}

// AssetRef is a minimal asset reference for JSON.
type AssetRef struct {
	Code string `json:"code"`
}

// StatementTransaction is a single transaction line in the statement.
type StatementTransaction struct {
	ID                 string  `json:"id"`
	CreatedAt          string  `json:"created_at"`
	Type               string  `json:"type"` // "credit" or "debit"
	Amount             string  `json:"amount"`
	CounterpartyAddress string `json:"counterparty_address"`
	CounterpartyName   string  `json:"counterparty_name,omitempty"`
	WalletID           string  `json:"wallet_id,omitempty"`
}

// StatementTotals holds the totals section.
type StatementTotals struct {
	TotalDebits  string `json:"total_debits"`
	TotalCredits string `json:"total_credits"`
	Balance      string `json:"balance"`
}

// StatementService generates account statements from Horizon and DB.
type StatementService struct {
	HorizonClient             horizonclient.ClientInterface
	DistributionAccountSvc   DistributionAccountServiceInterface
	Models                    *data.Models
}

// NewStatementService creates a new StatementService.
func NewStatementService(
	horizonClient horizonclient.ClientInterface,
	distSvc DistributionAccountServiceInterface,
	models *data.Models,
) *StatementService {
	return &StatementService{
		HorizonClient:           horizonClient,
		DistributionAccountSvc: distSvc,
		Models:                  models,
	}
}

var _ StatementServiceInterface = (*StatementService)(nil)

// GetStatement returns the statement for the given account, asset, and date range.
func (s *StatementService) GetStatement(ctx context.Context, account *schema.TransactionAccount, assetCode string, fromDate, toDate time.Time) (*StatementResult, error) {
	if !account.IsStellar() {
		return nil, ErrStatementAccountNotStellar
	}

	asset, err := s.resolveAsset(ctx, assetCode)
	if err != nil {
		return nil, err
	}

	endingBalance, err := s.DistributionAccountSvc.GetBalance(ctx, account, *asset)
	if err != nil {
		if errors.Is(err, ErrNoBalanceForAsset) {
			return nil, ErrStatementAssetNotFound
		}
		return nil, fmt.Errorf("getting balance: %w", err)
	}

	fromStart := time.Date(fromDate.Year(), fromDate.Month(), fromDate.Day(), 0, 0, 0, 0, time.UTC)
	toEnd := time.Date(toDate.Year(), toDate.Month(), toDate.Day(), 23, 59, 59, 999999999, time.UTC)

	transactions, totalCredits, totalDebits, involvedWalletIDs, err := s.fetchTransactionsInRange(ctx, account.Address, asset, fromStart, toEnd)
	if err != nil {
		return nil, err
	}

	// beginning_balance = ending_balance - total_credits + total_debits (replay)
	beginningBalance := endingBalance.Sub(totalCredits).Add(totalDebits)
	if beginningBalance.LessThan(decimal.Zero) {
		beginningBalance = decimal.Zero
	}

	assetCodeDisplay := asset.Code
	if asset.IsNative() {
		assetCodeDisplay = assets.XLMAssetCode
	}

	return &StatementResult{
		Summary: StatementSummary{
			Account:           "stellar:" + account.Address,
			Asset:              AssetRef{Code: assetCodeDisplay},
			BeginningBalance:   formatStellarAmount(beginningBalance),
			TotalCredits:       formatStellarAmount(totalCredits),
			TotalDebits:        formatStellarAmount(totalDebits),
			EndingBalance:      formatStellarAmount(endingBalance),
			InvolvedWalletIDs:  involvedWalletIDs,
		},
		Transactions: transactions,
		Totals: StatementTotals{
			TotalDebits:  formatStellarAmount(totalDebits),
			TotalCredits: formatStellarAmount(totalCredits),
			Balance:      formatStellarAmount(endingBalance),
		},
	}, nil
}

func (s *StatementService) resolveAsset(ctx context.Context, assetCode string) (*data.Asset, error) {
	code := assetCode
	if code == assets.XLMAssetCodeAlias {
		code = assets.XLMAssetCode
	}
	if code == assets.XLMAssetCode {
		return &data.Asset{Code: assets.XLMAssetCode, Issuer: ""}, nil
	}
	// Non-native: look up by code; use empty issuer to match any issuer, or get first by code from GetAll
	all, err := s.Models.Assets.GetAll(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing assets: %w", err)
	}
	for i := range all {
		if all[i].Code == code {
			return &all[i], nil
		}
	}
	return nil, ErrStatementAssetNotFound
}

func (s *StatementService) fetchTransactionsInRange(
	ctx context.Context,
	accountAddress string,
	asset *data.Asset,
	fromStart, toEnd time.Time,
) ([]StatementTransaction, decimal.Decimal, decimal.Decimal, []string, error) {
	var transactions []StatementTransaction
	var totalCredits, totalDebits decimal.Decimal
	walletIDSet := make(map[string]struct{})

	req := horizonclient.TransactionRequest{
		ForAccount: accountAddress,
		Order:      horizonclient.OrderAsc,
		Limit:      StatementTransactionsPageLimit,
	}
	pageCount := 0

	for {
		if pageCount >= MaxStatementTransactionsPages {
			break
		}
		pageCount++

		page, err := s.HorizonClient.Transactions(req)
		if err != nil {
			return nil, decimal.Zero, decimal.Zero, nil, fmt.Errorf("fetching transactions: %w", err)
		}

		for i := range page.Embedded.Records {
			tx := page.Embedded.Records[i]
			createdAt := tx.LedgerCloseTime
			if createdAt.Before(fromStart) {
				continue
			}
			if createdAt.After(toEnd) {
				// no need to fetch more pages
				return transactions, totalCredits, totalDebits, mapKeysToSlice(walletIDSet), nil
			}

			lines, credits, debits, walletIDs, err := s.processTransaction(ctx, &tx, accountAddress, asset)
			if err != nil {
				return nil, decimal.Zero, decimal.Zero, nil, err
			}
			transactions = append(transactions, lines...)
			totalCredits = totalCredits.Add(credits)
			totalDebits = totalDebits.Add(debits)
			for _, id := range walletIDs {
				walletIDSet[id] = struct{}{}
			}
		}

		if len(page.Embedded.Records) < StatementTransactionsPageLimit {
			break
		}
		nextPage, err := s.HorizonClient.NextTransactionsPage(page)
		if err != nil {
			return nil, decimal.Zero, decimal.Zero, nil, fmt.Errorf("next transactions page: %w", err)
		}
		if len(nextPage.Embedded.Records) == 0 {
			break
		}
		req.Cursor = nextPage.Embedded.Records[0].PT
	}

	return transactions, totalCredits, totalDebits, mapKeysToSlice(walletIDSet), nil
}

func (s *StatementService) processTransaction(
	ctx context.Context,
	tx *horizon.Transaction,
	accountAddress string,
	asset *data.Asset,
) ([]StatementTransaction, decimal.Decimal, decimal.Decimal, []string, error) {
	opsPage, err := s.HorizonClient.Operations(horizonclient.OperationRequest{ForTransaction: tx.Hash})
	if err != nil {
		return nil, decimal.Zero, decimal.Decimal{}, nil, fmt.Errorf("fetching operations for tx %s: %w", tx.Hash, err)
	}

	var lines []StatementTransaction
	var credits, debits decimal.Decimal
	var walletIDs []string
	createdAtStr := tx.LedgerCloseTime.UTC().Format(time.RFC3339)

	for i := range opsPage.Embedded.Records {
		op := opsPage.Embedded.Records[i]
		payment, ok := op.(operations.Payment)
		if !ok {
			continue
		}
		if !assetMatchesHorizonAsset(asset, payment.Asset) {
			continue
		}

		amount, err := decimal.NewFromString(payment.Amount)
		if err != nil {
			continue
		}
		if amount.LessThanOrEqual(decimal.Zero) {
			continue
		}

		var txType string
		var counterparty string
		if payment.From == accountAddress {
			txType = "debit"
			counterparty = payment.To
			debits = debits.Add(amount)
		} else {
			txType = "credit"
			counterparty = payment.From
			credits = credits.Add(amount)
		}

		name, walletID := s.resolveCounterparty(ctx, counterparty)
		if walletID != "" {
			walletIDs = append(walletIDs, walletID)
		}

		lines = append(lines, StatementTransaction{
			ID:                  tx.Hash,
			CreatedAt:           createdAtStr,
			Type:                txType,
			Amount:              payment.Amount,
			CounterpartyAddress:  counterparty,
			CounterpartyName:    name,
			WalletID:            walletID,
		})
	}

	return lines, credits, debits, walletIDs, nil
}

func assetMatchesHorizonAsset(asset *data.Asset, h base.Asset) bool {
	if asset.IsNative() && h.Type == "native" {
		return true
	}
	return asset.Code == h.Code && (asset.Issuer == h.Issuer || (asset.Issuer == "" && h.Issuer == ""))
}

func (s *StatementService) resolveCounterparty(ctx context.Context, stellarAddress string) (name, walletID string) {
	rw, err := s.Models.ReceiverWallet.GetByStellarAddress(ctx, stellarAddress)
	if err != nil {
		return "", ""
	}
	name = rw.Receiver.Email
	if name == "" {
		name = rw.Receiver.PhoneNumber
	}
	if name == "" {
		name = rw.Receiver.ExternalID
	}
	return name, rw.Wallet.ID
}

func formatStellarAmount(d decimal.Decimal) string {
	return d.StringFixed(7)
}

func mapKeysToSlice(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
