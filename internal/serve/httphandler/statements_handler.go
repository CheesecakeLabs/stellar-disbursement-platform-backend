package httphandler

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/stellar/go-stellar-sdk/support/render/httpjson"

	"github.com/stellar/stellar-disbursement-platform-backend/internal/data"
	"github.com/stellar/stellar-disbursement-platform-backend/internal/pdf"
	"github.com/stellar/stellar-disbursement-platform-backend/internal/serve/httperror"
	"github.com/stellar/stellar-disbursement-platform-backend/internal/serve/validators"
	"github.com/stellar/stellar-disbursement-platform-backend/internal/services"
	"github.com/stellar/stellar-disbursement-platform-backend/internal/transactionsubmission/engine/signing"
)

const errStatementOnlySupportedForStellar = "Statement is only supported for Stellar distribution accounts"

// StatementsHandler handles GET /statements for the account statement endpoint.
type StatementsHandler struct {
	DistributionAccountResolver signing.DistributionAccountResolver
	StatementService            services.StatementServiceInterface
	StatementQueryValidator     *validators.StatementQueryValidator
	Models                      *data.Models
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
		httperror.BadRequest(errStatementOnlySupportedForStellar, nil, nil).Render(w)
		return
	}

	result, err := h.StatementService.GetStatement(ctx, &distAccount, params.AssetCode, params.FromDate, params.ToDate)
	if err != nil {
		switch {
		case errors.Is(err, services.ErrStatementAccountNotStellar):
			httperror.BadRequest(errStatementOnlySupportedForStellar, err, nil).Render(w)
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

// GetStatementExport returns the statement as a PDF for the authenticated tenant's distribution account.
func (h StatementsHandler) GetStatementExport(w http.ResponseWriter, r *http.Request) {
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
		httperror.BadRequest(errStatementOnlySupportedForStellar, nil, nil).Render(w)
		return
	}

	result, err := h.StatementService.GetStatement(ctx, &distAccount, params.AssetCode, params.FromDate, params.ToDate)
	if err != nil {
		switch {
		case errors.Is(err, services.ErrStatementAccountNotStellar):
			httperror.BadRequest(errStatementOnlySupportedForStellar, err, nil).Render(w)
			return
		case errors.Is(err, services.ErrStatementAssetNotFound):
			httperror.NotFound("asset not found for account", err, nil).Render(w)
			return
		default:
			httperror.InternalError(ctx, "Cannot retrieve statement", err, nil).Render(w)
			return
		}
	}

	var orgName string
	var orgLogo []byte
	if h.Models != nil {
		if org, err := h.Models.Organizations.Get(ctx); err == nil {
			orgName = org.Name
			orgLogo = org.Logo
		}
	}

	pdfBytes, err := pdf.BuildPDF(result, params.FromDate, params.ToDate, orgName, orgLogo)
	if err != nil {
		httperror.InternalError(ctx, "Cannot generate statement PDF", err, nil).Render(w)
		return
	}

	filename := fmt.Sprintf("statement_%s-%s.pdf",
		params.FromDate.Format("20060102"),
		params.ToDate.Format("20060102"))
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s", filename))
	if _, err := w.Write(pdfBytes); err != nil {
		httperror.InternalError(ctx, "Cannot write statement PDF", err, nil).Render(w)
		return
	}
}
