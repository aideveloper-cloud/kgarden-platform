package erpnext

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client is a thin REST client for the parts of ERPNext the K Garden services need.
// It authenticates as the integration user with an API key/secret.
type Client struct {
	base   string
	auth   string
	client *http.Client
}

func New(base, key, secret string) *Client {
	return &Client{
		base:   strings.TrimRight(base, "/"),
		auth:   "token " + key + ":" + secret,
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

// Error carries the exception type ERPNext reported, e.g. "UniqueValidationError".
type Error struct {
	Status  int
	ExcType string
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("erpnext %d %s: %s", e.Status, e.ExcType, e.Message)
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, body any, out any) error {
	u := c.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", c.auth)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		return parseError(resp.StatusCode, raw)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

func parseError(status int, raw []byte) error {
	var body struct {
		ExcType        string `json:"exc_type"`
		Exception      string `json:"exception"`
		ServerMessages string `json:"_server_messages"`
	}
	_ = json.Unmarshal(raw, &body)
	msg := body.Exception
	if body.ServerMessages != "" {
		var msgs []string
		if json.Unmarshal([]byte(body.ServerMessages), &msgs) == nil && len(msgs) > 0 {
			var m struct {
				Message string `json:"message"`
			}
			if json.Unmarshal([]byte(msgs[0]), &m) == nil && m.Message != "" {
				msg = m.Message
			}
		}
	}
	if msg == "" {
		msg = strings.TrimSpace(string(raw))
		if len(msg) > 300 {
			msg = msg[:300]
		}
	}
	return &Error{Status: status, ExcType: body.ExcType, Message: msg}
}

type WorkOrder struct {
	Name           string      `json:"name"`
	ProductionItem string      `json:"production_item"`
	ItemName       string      `json:"item_name"`
	BOMNo          string      `json:"bom_no"`
	Qty            json.Number `json:"qty"`
	ProducedQty    json.Number `json:"produced_qty"`
	StockUOM       string      `json:"stock_uom"`
	Status         string      `json:"status"`
	Modified       string      `json:"modified"`
}

var workOrderFields = `["name","production_item","item_name","bom_no","qty","produced_qty","stock_uom","status","modified"]`

// ListWorkOrders returns submitted Work Orders, newest first.
func (c *Client) ListWorkOrders(ctx context.Context) ([]WorkOrder, error) {
	q := url.Values{
		"fields":            {workOrderFields},
		"filters":           {`[["docstatus","=",1]]`},
		"order_by":          {"modified desc"},
		"limit_page_length": {"500"},
	}
	var out struct {
		Data []WorkOrder `json:"data"`
	}
	err := c.do(ctx, http.MethodGet, "/api/resource/Work Order", q, nil, &out)
	return out.Data, err
}

type StockEntryRef struct {
	Name      string `json:"name"`
	DocStatus int    `json:"docstatus"`
}

// FindStockEntryByKey looks up a Stock Entry by the K Garden idempotency key.
func (c *Client) FindStockEntryByKey(ctx context.Context, key string) (*StockEntryRef, error) {
	filters, _ := json.Marshal([][]string{{"kg_idempotency_key", "=", key}})
	q := url.Values{"fields": {`["name","docstatus"]`}, "filters": {string(filters)}}
	var out struct {
		Data []StockEntryRef `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/resource/Stock Entry", q, nil, &out); err != nil {
		return nil, err
	}
	if len(out.Data) == 0 {
		return nil, nil
	}
	return &out.Data[0], nil
}

// MakeManufactureEntry asks ERPNext to build (not save) a Manufacture Stock Entry for a Work Order,
// with raw materials backflushed from the BOM exactly as the Desk "Finish" button does.
func (c *Client) MakeManufactureEntry(ctx context.Context, workOrder, qty string) (map[string]any, error) {
	body := map[string]any{"work_order_id": workOrder, "purpose": "Manufacture", "qty": json.Number(qty)}
	var out struct {
		Message map[string]any `json:"message"`
	}
	err := c.do(ctx, http.MethodPost, "/api/method/erpnext.manufacturing.doctype.work_order.mapper.make_stock_entry", nil, body, &out)
	return out.Message, err
}

// InsertSubmitted inserts a document with docstatus=1, which ERPNext saves and submits in one call.
func (c *Client) InsertSubmitted(ctx context.Context, doctype string, doc map[string]any) (map[string]any, error) {
	doc = cleanDoc(doc).(map[string]any)
	doc["docstatus"] = 1
	var out struct {
		Data map[string]any `json:"data"`
	}
	err := c.do(ctx, http.MethodPost, "/api/resource/"+url.PathEscape(doctype), nil, doc, &out)
	return out.Data, err
}

// Submit moves an existing draft to docstatus=1.
func (c *Client) Submit(ctx context.Context, doctype, name string) (map[string]any, error) {
	var out struct {
		Data map[string]any `json:"data"`
	}
	err := c.do(ctx, http.MethodPut, "/api/resource/"+url.PathEscape(doctype)+"/"+url.PathEscape(name), nil, map[string]any{"docstatus": 1}, &out)
	return out.Data, err
}

func (c *Client) Get(ctx context.Context, doctype, name string) (map[string]any, error) {
	var out struct {
		Data map[string]any `json:"data"`
	}
	err := c.do(ctx, http.MethodGet, "/api/resource/"+url.PathEscape(doctype)+"/"+url.PathEscape(name), nil, nil, &out)
	return out.Data, err
}

// cleanDoc drops client-side bookkeeping keys (__islocal, unsaved names) from a document built by
// a mapper before it is sent back for insert.
func cleanDoc(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			if strings.HasPrefix(k, "__") || ((k == "name" || k == "parent") && val == nil) {
				continue
			}
			out[k] = cleanDoc(val)
		}
		return out
	case []any:
		for i := range t {
			t[i] = cleanDoc(t[i])
		}
		return t
	default:
		return v
	}
}

// List returns documents of doctype matching filters (Frappe filter triples), with the given fields.
func (c *Client) List(ctx context.Context, doctype string, fields []string, filters [][]any, limit int) ([]map[string]any, error) {
	f, _ := json.Marshal(fields)
	flt, _ := json.Marshal(filters)
	q := url.Values{"fields": {string(f)}, "filters": {string(flt)}, "limit_page_length": {fmt.Sprint(limit)}}
	var out struct {
		Data []map[string]any `json:"data"`
	}
	err := c.do(ctx, http.MethodGet, "/api/resource/"+url.PathEscape(doctype), q, nil, &out)
	return out.Data, err
}

// Exists reports whether a document with this name exists.
func (c *Client) Exists(ctx context.Context, doctype, name string) (bool, error) {
	_, err := c.Get(ctx, doctype, name)
	var ee *Error
	if errors.As(err, &ee) && ee.Status == http.StatusNotFound {
		return false, nil
	}
	return err == nil, err
}

// Insert saves a new document (draft for submittable doctypes) and returns it.
func (c *Client) Insert(ctx context.Context, doctype string, doc map[string]any) (map[string]any, error) {
	var out struct {
		Data map[string]any `json:"data"`
	}
	err := c.do(ctx, http.MethodPost, "/api/resource/"+url.PathEscape(doctype), nil, doc, &out)
	return out.Data, err
}

// IsDuplicate reports whether err is ERPNext refusing a second document with the same unique value.
func IsDuplicate(err error) bool {
	var ee *Error
	return errors.As(err, &ee) && (ee.ExcType == "UniqueValidationError" || ee.ExcType == "DuplicateEntryError")
}

// IsNegativeStock reports whether ERPNext refused a stock movement because the stock is not there
// (yet). Callers usually retry: the movement that brings the stock may still be on its way.
func IsNegativeStock(err error) bool {
	var ee *Error
	return errors.As(err, &ee) && ee.ExcType == "NegativeStockError"
}

// Update changes fields of an existing document and returns it.
func (c *Client) Update(ctx context.Context, doctype, name string, fields map[string]any) (map[string]any, error) {
	var out struct {
		Data map[string]any `json:"data"`
	}
	err := c.do(ctx, http.MethodPut, "/api/resource/"+url.PathEscape(doctype)+"/"+url.PathEscape(name), nil, fields, &out)
	return out.Data, err
}

// Cancel cancels a submitted document (docstatus 1 -> 2).
func (c *Client) Cancel(ctx context.Context, doctype, name string) error {
	return c.do(ctx, http.MethodPost, "/api/method/frappe.client.cancel", nil,
		map[string]any{"doctype": doctype, "name": name}, nil)
}

// FindByField returns the name and docstatus of the first document whose field equals value.
func (c *Client) FindByField(ctx context.Context, doctype, field, value string) (name string, docstatus int, err error) {
	rows, err := c.List(ctx, doctype, []string{"name", "docstatus"}, [][]any{{field, "=", value}}, 1)
	if err != nil || len(rows) == 0 {
		return "", 0, err
	}
	ds, _ := rows[0]["docstatus"].(float64)
	return rows[0]["name"].(string), int(ds), nil
}
