package httphandler

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/stellar/stellar-disbursement-platform-backend/internal/services"
	"github.com/stellar/stellar-disbursement-platform-backend/internal/serve/validators"
	sigMocks "github.com/stellar/stellar-disbursement-platform-backend/internal/transactionsubmission/engine/signing/mocks"
	"github.com/stellar/stellar-disbursement-platform-backend/pkg/schema"
)

const testQueryParams = "?asset_code=XLM&from_date=2026-01-01&to_date=2026-01-31"
const emptyBalance = "0.0000000"

type mockStatementService struct {
	mock.Mock
}

func (m *mockStatementService) GetStatement(ctx context.Context, account *schema.TransactionAccount, assetCode string, fromDate, toDate time.Time) (*services.StatementResult, error) {
	args := m.Called(ctx, account, assetCode, fromDate, toDate)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*services.StatementResult), args.Error(1)
}

func TestStatementsHandlerGetStatement(t *testing.T) {
	stellarAccount := schema.TransactionAccount{
		Address: "GDNRRK5EXMZ4STV7UTO3CW4LSVNY5KYWTM3J7BM5SQNA7KE2RYX55IYV",
		Type:    schema.DistributionAccountStellarEnv,
	}
	successResult := &services.StatementResult{
		Summary: services.StatementSummary{
			Account:           "stellar:GDNRRK5EXMZ4STV7UTO3CW4LSVNY5KYWTM3J7BM5SQNA7KE2RYX55IYV",
			Asset:             services.AssetRef{Code: "XLM"},
			BeginningBalance:  emptyBalance,
			TotalCredits:      emptyBalance,
			TotalDebits:       emptyBalance,
			EndingBalance:     "9.7998900",
			InvolvedWalletIDs: []string{"07815404-eb0d-4188-a362-38a90aae185c"},
		},
		Transactions: []services.StatementTransaction{},
		Totals: services.StatementTotals{
			TotalDebits:  emptyBalance,
			TotalCredits: emptyBalance,
			Balance:      "9.7998900",
		},
	}

	testCases := []struct {
		name             string
		query            string
		prepareMocks     func(*mockStatementService, *sigMocks.MockDistributionAccountResolver)
		expectedStatus   int
		expectedContains string
	}{
		{
			name:   "returns 400 when asset_code is missing",
			query:  "?from_date=2026-01-01&to_date=2026-01-31",
			prepareMocks: func(_ *mockStatementService, _ *sigMocks.MockDistributionAccountResolver) {},
			expectedStatus:   http.StatusBadRequest,
			expectedContains: "asset_code",
		},
		{
			name:   "returns 400 when from_date is missing",
			query:  "?asset_code=XLM&to_date=2026-01-31",
			prepareMocks: func(_ *mockStatementService, _ *sigMocks.MockDistributionAccountResolver) {},
			expectedStatus:   http.StatusBadRequest,
			expectedContains: "from_date",
		},
		{
			name:   "returns 400 when to_date is missing",
			query:  "?asset_code=XLM&from_date=2026-01-01",
			prepareMocks: func(_ *mockStatementService, _ *sigMocks.MockDistributionAccountResolver) {},
			expectedStatus:   http.StatusBadRequest,
			expectedContains: "to_date",
		},
		{
			name:   "returns 500 when distribution account resolver fails",
			query:  testQueryParams,
			prepareMocks: func(_ *mockStatementService, mResolver *sigMocks.MockDistributionAccountResolver) {
				mResolver.On("DistributionAccountFromContext", mock.Anything).
					Return(schema.TransactionAccount{}, errors.New("resolver error")).
					Once()
			},
			expectedStatus:   http.StatusInternalServerError,
			expectedContains: "Cannot retrieve distribution account",
		},
		{
			name:   "returns 400 when account is not Stellar",
			query:  testQueryParams,
			prepareMocks: func(_ *mockStatementService, mResolver *sigMocks.MockDistributionAccountResolver) {
				mResolver.On("DistributionAccountFromContext", mock.Anything).
					Return(schema.TransactionAccount{Type: schema.DistributionAccountCircleDBVault}, nil).
					Once()
			},
			expectedStatus:   http.StatusBadRequest,
			expectedContains: "only supported for Stellar",
		},
		{
			name:   "returns 404 when asset not found for account",
			query:  testQueryParams,
			prepareMocks: func(mSvc *mockStatementService, mResolver *sigMocks.MockDistributionAccountResolver) {
				mResolver.On("DistributionAccountFromContext", mock.Anything).
					Return(stellarAccount, nil).
					Once()
				mSvc.On("GetStatement", mock.Anything, mock.MatchedBy(func(a *schema.TransactionAccount) bool {
					return a != nil && a.Address == stellarAccount.Address && a.Type == stellarAccount.Type
				}), "XLM",
					time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
					time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC)).
					Return(nil, services.ErrStatementAssetNotFound).
					Once()
			},
			expectedStatus:   http.StatusNotFound,
			expectedContains: "asset not found",
		},
		{
			name:   "returns 500 when statement service fails with unexpected error",
			query:  testQueryParams,
			prepareMocks: func(mSvc *mockStatementService, mResolver *sigMocks.MockDistributionAccountResolver) {
				mResolver.On("DistributionAccountFromContext", mock.Anything).
					Return(stellarAccount, nil).
					Once()
				mSvc.On("GetStatement", mock.Anything, mock.MatchedBy(func(a *schema.TransactionAccount) bool {
					return a != nil && a.Address == stellarAccount.Address && a.Type == stellarAccount.Type
				}), "XLM",
					time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
					time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC)).
					Return(nil, errors.New("horizon error")).
					Once()
			},
			expectedStatus:   http.StatusInternalServerError,
			expectedContains: "Cannot retrieve statement",
		},
		{
			name:   "returns 200 and JSON shape on success",
			query:  testQueryParams,
			prepareMocks: func(mSvc *mockStatementService, mResolver *sigMocks.MockDistributionAccountResolver) {
				mResolver.On("DistributionAccountFromContext", mock.Anything).
					Return(stellarAccount, nil).
					Once()
				mSvc.On("GetStatement", mock.Anything, mock.MatchedBy(func(a *schema.TransactionAccount) bool {
					return a != nil && a.Address == stellarAccount.Address && a.Type == stellarAccount.Type
				}), "XLM",
					time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
					time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC)).
					Return(successResult, nil).
					Once()
			},
			expectedStatus:   http.StatusOK,
			expectedContains: "summary",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			mSvc := &mockStatementService{}
			mResolver := sigMocks.NewMockDistributionAccountResolver(t)
			tc.prepareMocks(mSvc, mResolver)

			h := StatementsHandler{
				DistributionAccountResolver: mResolver,
				StatementService:            mSvc,
				StatementQueryValidator:     validators.NewStatementQueryValidator(),
			}

			rr := httptest.NewRecorder()
			req, err := http.NewRequest(http.MethodGet, "/statements"+tc.query, nil)
			require.NoError(t, err)
			http.HandlerFunc(h.GetStatement).ServeHTTP(rr, req)
			resp := rr.Result()
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)

			assert.Equal(t, tc.expectedStatus, resp.StatusCode)
			assert.Contains(t, string(body), tc.expectedContains)

			if tc.expectedStatus == http.StatusOK {
				assert.Contains(t, string(body), `"summary"`)
				assert.Contains(t, string(body), `"transactions"`)
				assert.Contains(t, string(body), `"totals"`)
				assert.Contains(t, string(body), `"involved_wallet_ids"`)
			}
		})
	}
}
