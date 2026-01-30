package httphandler

import (
	"errors"
	"net/http"

	"github.com/stellar/go-stellar-sdk/support/render/httpjson"

	"github.com/stellar/stellar-disbursement-platform-backend/internal/serve/httperror"
	"github.com/stellar/stellar-disbursement-platform-backend/internal/serve/validators"
	"github.com/stellar/stellar-disbursement-platform-backend/internal/services"
	"github.com/stellar/stellar-disbursement-platform-backend/internal/transactionsubmission/engine/signing"
)

// StatementsHandler handles GET /statements for the account statement endpoint.
type StatementsHandler struct {
	DistributionAccountResolver signing.DistributionAccountResolver
	StatementService            services.StatementServiceInterface
	StatementQueryValidator     *validators.StatementQueryValidator
}

// GetStatement returns the statement for the authenticated tenant's distribution account.
func (h StatementsHandler) GetStatement(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	params := h.StatementQueryValidator.ValidateAndGetStatementParams(r)
	if h.StatementQueryValidator.HasErrors() {
		httperror.BadRequest("invalid query parameters", nil, h.StatementQueryValidator.Validator.Errors).Render(w)
		return
	}

	distAccount, err := h.DistributionAccountResolver.DistributionAccountFromContext(ctx)
	if err != nil {
		httperror.InternalError(ctx, "Cannot retrieve distribution account", err, nil).Render(w)
		return
	}

	if !distAccount.IsStellar() {
		httperror.BadRequest("Statement is only supported for Stellar distribution accounts", nil, nil).Render(w)
		return
	}

	result, err := h.StatementService.GetStatement(ctx, &distAccount, params.AssetCode, params.FromDate, params.ToDate)
	if err != nil {
		switch {
		case errors.Is(err, services.ErrStatementAccountNotStellar):
			httperror.BadRequest("Statement is only supported for Stellar distribution accounts", err, nil).Render(w)
			return
		case errors.Is(err, services.ErrStatementAssetNotFound):
			httperror.NotFound("asset not found for account", err, nil).Render(w)
			return
		default:
			httperror.InternalError(ctx, "Cannot retrieve statement", err, nil).Render(w)
			return
		}
	}

	httpjson.Render(w, result, httpjson.JSON)
}
