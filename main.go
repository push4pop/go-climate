package conversion

import (
	"context"
	"net/http"
	"strconv"

	"fmt"

	"github.com/bukalapak/annex/transaction"

	"github.com/bukalapak/annex"
	"github.com/bukalapak/annex/authentication"
	"github.com/bukalapak/annex/errors"
	"github.com/bukalapak/annex/server/request"
	"github.com/bukalapak/annex/server/response"
	"github.com/julienschmidt/httprouter"
)

// ExclusiveHandler handle public request
type ExclusiveHandler struct{}

// GetConversionCount counts conversion of an hasoffer transaction
func (h *ExclusiveHandler) GetConversionCount(w http.ResponseWriter, r *http.Request, ps httprouter.Params) error {
	ctx := r.Context()
	ctx = annex.AppendTags(ctx, "conversion")

	trxID := ps.ByName("trxID")
	count, err := CountConversion(ctx, trxID)
	if err != nil {
		response.Error(w, errors.ConversionNotFound)
		return errors.ConversionNotFound
	}
	response.OK(w, struct {
		ConversionCount int `json:"conversion_count"`
	}{count})

	return nil
}

// SendPostback to hasoffer
func (h *ExclusiveHandler) SendPostback(w http.ResponseWriter, r *http.Request, ps httprouter.Params) error {
	ctx := r.Context()
	ctx = annex.AppendTags(ctx, "conversion")

	_, err := authentication.Token(r.Header.Get("Authorization"), []string{})
	if err != nil {
		response.Error(w, errors.Unauthorized)
		return err
	}

	trxID := ps.ByName("trxID")
	bukalapakTrxID, err := strconv.Atoi(trxID)
	ctx = annex.SetTrackID(ctx, trxID)

	if err != nil {
		response.Error(w, errors.UnprocessableEntity)
		return err
	}

	conv, err := FindByBukalapakTransactionID(ctx, uint(bukalapakTrxID))
	if err != nil {
		response.Error(w, errors.ConversionNotFound)
		return errors.ConversionNotFound
	}

	trxLog := annex.BukalapakTransaction{}
	mothershipTrx, err := CheckMothershipTransaction(conv.TransactionType, conv.BukalapakTransactionID)
	if err != nil {
		response.Error(w, err)
		return err
	}
	trxLog.ConversionID = &conv.ID
	trxLog.InvoiceNumber = mothershipTrx.InvoiceNumber
	trxLog.Amount = mothershipTrx.Amount

	trxLog.PaymentMethod = mothershipTrx.PaymentMethod
	if err = trxLog.RemitValidation(); err != nil {
		annex.Logger.Errorf("Error remit: %v\n", err)
		response.Error(w, err)
		trxLog.Status = "rejected"
		transaction.Update(ctx, trxLog)
		return err
	}

	if conv.Status == "completed" && conv.RemittedAt != nil || conv.Status == "failed" {
		annex.Logger.NewInfof(ctx, "Conversion with Bukalapak Transaction ID %v and Hasoffer Transaction ID %v is already used before", conv.BukalapakTransactionID, conv.TransactionID)
		response.OK(w, conv)
		return nil
	}

	if mothershipTrx.RemittedAt == nil {
		annex.Logger.NewInfof(ctx, "Conversion with Bukalapak Transaction ID %v and Hasoffer Transaction ID %v is not remitted. Transaction state: %v", conv.BukalapakTransactionID, conv.TransactionID, mothershipTrx.State)
		response.Error(w, errors.TransactionNotRemitted)
		return errors.TransactionNotRemitted
	}

	conv.Amount = mothershipTrx.Amount
	conv.RemittedAt = mothershipTrx.RemittedAt
	conv.InvoiceNumber = mothershipTrx.InvoiceNumber

	updated, _ := Remit(ctx, conv, trxLog)

	response.OK(w, updated)
	return nil
}

// GetConversions used to retrieve conversions
func (h *ExclusiveHandler) GetConversions(w http.ResponseWriter, r *http.Request, _ httprouter.Params) error {
	ctx := r.Context()
	ctx = annex.AppendTags(ctx, "conversion")

	_, err := authentication.Token(r.Header.Get("Authorization"), []string{})
	if err != nil {
		response.Error(w, errors.Unauthorized)
		return err
	}

	filter := make(map[string]string)
	params := r.URL.Query()
	if start_date, ok := params["start_date"]; ok && start_date[0] != "" {
		filter["start_date"] = start_date[0]
	} else {
		filter["start_date"] = "0001-01-01"
	}

	if end_date, ok := params["end_date"]; ok && end_date[0] != "" {
		filter["end_date"] = end_date[0]
	} else {
		filter["end_date"] = "9999-12-31"
	}

	if search_by, ok := params["search_by"]; ok && search_by[0] != "" {
		filter["search_by"] = search_by[0]
	} else {
		filter["search_by"] = ""
	}

	if keywords, ok := params["keywords"]; ok {
		filter["keywords"] = keywords[0]
	} else {
		filter["keywords"] = ""
	}

	var limit int
	if l, ok := params["limit"]; ok {
		if limit, err = strconv.Atoi(l[0]); err != nil {
			response.Error(w, errors.UnprocessableEntity)
			return err
		}
	} else {
		limit = 10
	}

	var offset int
	if o, ok := params["offset"]; ok {
		if offset, err = strconv.Atoi(o[0]); err != nil {
			response.Error(w, errors.UnprocessableEntity)
			return err
		}
	} else {
		offset = 0
	}

	if sort, ok := params["sort"]; ok {
		filter["sort"] = sort[0]
	} else {
		filter["sort"] = "desc"
	}

	conversions, count, err := RetrieveConversions(ctx, filter, limit, offset)
	if err != nil {
		response.Error(w, err)
		return err
	}

	meta := response.Meta{
		HTTPStatus: 200,
		Limit:      limit,
		Offset:     offset,
		Total:      count,
	}
	response.OKMeta(w, conversions, meta)
	return nil
}

// CreateManualConversion by admin or marketing
func (h *ExclusiveHandler) CreateManualConversion(w http.ResponseWriter, r *http.Request, ps httprouter.Params) error {
	ctx := r.Context()
	ctx = annex.AppendTags(ctx, "conversion")

	var conv annex.Conversion
	err := request.Decode(r, &conv)
	if err != nil {
		response.Error(w, err)
		return err
	}
	ctx = annex.SetTrackID(ctx, fmt.Sprint(conv.BukalapakTransactionID))

	var trxLog annex.BukalapakTransaction
	err = request.Decode(r, &trxLog)
	if err != nil {
		response.Error(w, err)
		return err
	}

	err = conv.ValidateEmptyField()
	if err != nil {
		response.Error(w, err)
		return err
	}

	err = ManualCheck(ctx, conv)
	if err != nil {
		response.Error(w, err)
		return err
	}

	owner := ctx.Value("Auth-Owner").(annex.BukalapakTokenResourceOwner)

	var mustSuccess bool
	ctx = context.WithValue(ctx, "Must-Success", &mustSuccess)

	conv, err = Manual(ctx, conv, trxLog, owner)
	if err != nil {
		if mustSuccess {
			response.OK(w, err)
		} else {
			response.Error(w, err)
		}
		return err
	}

	response.Created(w, conv)
	return nil
}

