package controller

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"time"

	"isucon8/isubank"
	"isucon8/isucoin/model"

	"github.com/go-sql-driver/mysql"
	"github.com/gorilla/sessions"
	"github.com/julienschmidt/httprouter"
	"github.com/pkg/errors"
	"golang.org/x/crypto/bcrypt"
)

var (
	empty = struct{}{}
)

// structs

type errWithCode struct {
	StatusCode int
	Err        error
}

func (e *errWithCode) Error() string {
	return e.Err.Error()
}

func errcodeWrap(err error, code int) error {
	if err == nil {
		return nil
	}
	return &errWithCode{
		StatusCode: code,
		Err:        err,
	}
}

func errcode(err string, code int) error {
	return errcodeWrap(errors.New(err), code)
}

func NewServer(db *sql.DB, store sessions.Store, publicdir, datadir string) http.Handler {

	h := &Handler{
		db:      db,
		store:   store,
		datadir: datadir,
	}

	router := httprouter.New()
	router.POST("/initialize", h.Initialize)
	router.POST("/signup", h.Signup)
	router.POST("/signin", h.Signin)
	router.POST("/signout", h.Signout)
	router.GET("/info", h.Info)
	router.POST("/orders", h.AddOrders)
	router.GET("/orders", h.GetOrders)
	router.DELETE("/order/:id", h.DeleteOrders)
	router.NotFound = http.FileServer(http.Dir(publicdir)).ServeHTTP

	return h.commonHandler(router)
}

type Handler struct {
	db      *sql.DB
	store   sessions.Store
	datadir string
}

func (h *Handler) Initialize(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	cmd := exec.Command("sh", h.datadir+"/init")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		h.handleError(w, errors.Wrapf(err, "init.sh failed"), 500)
		return
	}
	err := txScorp(h.db, func(tx *sql.Tx) error {
		var dt time.Time
		if err := tx.QueryRow(`select max(created_at) from trade`).Scan(&dt); err != nil {
			return errors.Wrap(err, "get last traded")
		}
		diffmin := int64(time.Now().Sub(dt).Minutes())
		if _, err := tx.Exec("update trade set created_at = (created_at + interval ? minute)", diffmin); err != nil {
			return errors.Wrap(err, "update trade.created_at")
		}
		if _, err := tx.Exec("update orders set created_at = (created_at + interval ? minute)", diffmin); err != nil {
			return errors.Wrap(err, "update orders.created_at")
		}
		if _, err := tx.Exec("update orders set closed_at = (closed_at + interval ? minute) where closed_at is not null", diffmin); err != nil {
			return errors.Wrap(err, "update orders.closed_at")
		}
		query := `INSERT INTO setting (name, val) VALUES (?, ?) ON DUPLICATE KEY UPDATE val = VALUES(val)`
		for _, k := range []string{
			model.BankEndpoint,
			model.BankAppid,
			model.LogEndpoint,
			model.LogAppid,
		} {
			if _, err := tx.Exec(query, k, r.FormValue(k)); err != nil {
				return errors.Wrapf(err, "set setting failed. %s", k)
			}
		}
		return nil
	})
	if err != nil {
		h.handleError(w, err, 500)
	} else {
		time.Sleep(10 * time.Second)
		h.handleSuccess(w, empty)
	}
}

func (h *Handler) Signup(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	name := r.FormValue("name")
	bankID := r.FormValue("bank_id")
	password := r.FormValue("password")
	if name == "" || bankID == "" || password == "" {
		h.handleError(w, errors.New("all paramaters are required"), 400)
		return
	}
	bank, err := model.Isubank(h.db)
	if err != nil {
		h.handleError(w, err, 500)
		return
	}
	logger, err := model.Logger(h.db)
	if err != nil {
		h.handleError(w, err, 500)
		return
	}
	// bankIDの検証
	if err = bank.Check(bankID, 0); err != nil {
		h.handleError(w, err, 404)
		return
	}
	pass, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		h.handleError(w, err, 500)
		return
	}
	if res, err := h.db.Exec(`INSERT INTO user (bank_id, name, password, created_at) VALUES (?, ?, ?, NOW(6))`, bankID, name, pass); err != nil {
		if mysqlError, ok := err.(*mysql.MySQLError); ok {
			if mysqlError.Number == 1062 {
				h.handleError(w, errors.New("bank_id conflict"), 409)
				return
			}
		}
		h.handleError(w, err, 500)
		return
	} else {
		userID, _ := res.LastInsertId()
		err := logger.Send("signup", map[string]interface{}{
			"bank_id": bankID,
			"user_id": userID,
			"name":    name,
		})
		if err != nil {
			log.Printf("[WARN] logger.Send failed. err:%s", err)
		}
	}
	h.handleSuccess(w, empty)
}

func (h *Handler) Signin(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	bankID := r.FormValue("bank_id")
	password := r.FormValue("password")
	if bankID == "" || password == "" {
		h.handleError(w, errors.New("all paramaters are required"), 400)
		return
	}
	logger, err := model.Logger(h.db)
	if err != nil {
		h.handleError(w, err, 500)
		return
	}

	user, err := model.GetUserByBankID(h.db, bankID)
	if err != nil {
		if err == sql.ErrNoRows {
			h.handleError(w, errors.New("bank_id or password is not match"), 404)
			return
		}
		h.handleError(w, err, 500)
		return
	}
	if err := bcrypt.CompareHashAndPassword([]byte(user.Password), []byte(password)); err != nil {
		if err == bcrypt.ErrMismatchedHashAndPassword {
			h.handleError(w, errors.New("bank_id or password is not match"), 404)
			return
		}
		h.handleError(w, err, 400)
		return
	}
	session, err := h.store.Get(r, SessionName)
	if err != nil {
		h.handleError(w, err, 500)
		return
	}
	session.Values["user_id"] = user.ID
	if err = session.Save(r, w); err != nil {
		h.handleError(w, err, 500)
		return
	}
	err = logger.Send("signin", map[string]interface{}{
		"user_id": user.ID,
	})
	if err != nil {
		log.Printf("[WARN] logger.Send failed. err:%s", err)
	}
	h.handleSuccess(w, user)
}

func (h *Handler) Signout(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	session, err := h.store.Get(r, SessionName)
	if err != nil {
		h.handleError(w, err, 500)
		return
	}
	session.Values["user_id"] = 0
	session.Options = &sessions.Options{MaxAge: -1}
	if err = session.Save(r, w); err != nil {
		h.handleError(w, err, 500)
		return
	}
	h.handleSuccess(w, empty)
}

func (h *Handler) Info(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	var (
		err         error
		lastTradeID int64
		lt          = time.Unix(0, 0)
		res         = make(map[string]interface{}, 10)
	)
	if _cursor := r.URL.Query().Get("cursor"); _cursor != "" {
		lastTradeID, err = strconv.ParseInt(_cursor, 10, 64)
		if err != nil {
			h.handleError(w, errors.Wrap(err, "cursor parse failed"), 400)
			return
		}
		trade, err := model.GetTradeByID(h.db, lastTradeID)
		if err != nil && err != sql.ErrNoRows {
			h.handleError(w, errors.Wrap(err, "getTradeByID failed"), 500)
			return
		}
		if trade != nil {
			lt = trade.CreatedAt
		}
	}
	res["cursor"] = lastTradeID
	trades, err := model.GetTradesByLastID(h.db, lastTradeID)
	if err != nil {
		h.handleError(w, errors.Wrap(err, "getTradesByLastID failed"), 500)
		return
	}
	user, _ := h.userByRequest(r)
	if l := len(trades); l > 0 {
		res["cursor"] = trades[l-1].ID
		if user != nil {
			tradeIDs := make([]int64, len(trades))
			for i, trade := range trades {
				tradeIDs[i] = trade.ID
			}
			orders, err := model.GetOrdersByUserIDAndTradeIds(h.db, user.ID, tradeIDs)
			if err != nil {
				h.handleError(w, err, 500)
				return
			}
			for _, order := range orders {
				if err = model.FetchOrderRelation(h.db, order); err != nil {
					h.handleError(w, err, 500)
					return
				}
			}
			res["traded_orders"] = orders
		}
	}

	bySecTime := time.Date(lt.Year(), lt.Month(), lt.Day(), lt.Hour(), lt.Minute(), lt.Second(), 0, lt.Location())
	chartBySec, err := model.GetCandlestickData(h.db, bySecTime, "%Y-%m-%d %H:%i:%s")
	if err != nil {
		h.handleError(w, errors.Wrap(err, "model.GetCandlestickData by sec"), 500)
		return
	}
	res["chart_by_sec"] = chartBySec

	byMinTime := time.Date(lt.Year(), lt.Month(), lt.Day(), lt.Hour(), lt.Minute(), 0, 0, lt.Location())
	chartByMin, err := model.GetCandlestickData(h.db, byMinTime, "%Y-%m-%d %H:%i:00")
	if err != nil {
		h.handleError(w, errors.Wrap(err, "model.GetCandlestickData by min"), 500)
		return
	}
	res["chart_by_min"] = chartByMin

	byHourTime := time.Date(lt.Year(), lt.Month(), lt.Day(), lt.Hour(), 0, 0, 0, lt.Location())
	chartByHour, err := model.GetCandlestickData(h.db, byHourTime, "%Y-%m-%d %H:00:00")
	if err != nil {
		h.handleError(w, errors.Wrap(err, "model.GetCandlestickData by hour"), 500)
		return
	}
	res["chart_by_hour"] = chartByHour

	lowestSellOrder, err := model.GetLowestSellOrder(h.db)
	switch {
	case err == sql.ErrNoRows:
	case err != nil:
		h.handleError(w, errors.Wrap(err, "model.GetLowestSellOrder"), 500)
		return
	default:
		res["lowest_sell_price"] = lowestSellOrder.Price
	}

	highestBuyOrder, err := model.GetHighestBuyOrder(h.db)
	switch {
	case err == sql.ErrNoRows:
	case err != nil:
		h.handleError(w, errors.Wrap(err, "model.GetHighestBuyOrder"), 500)
		return
	default:
		res["highest_buy_price"] = highestBuyOrder.Price
	}
	res["enable_share"] = false

	h.handleSuccess(w, res)
}

func (h *Handler) AddOrders(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	user, err := h.userByRequest(r)
	if err != nil {
		h.handleError(w, err, 401)
		return
	}

	var id int64
	err = txScorp(h.db, func(tx *sql.Tx) error {
		if _, err := model.GetUserByIDWithLock(tx, user.ID); err != nil {
			return errors.Wrapf(err, "model.GetUserByIDWithLock failed. id:%d", user.ID)
		}
		logger, err := model.Logger(tx)
		if err != nil {
			return errors.Wrap(err, "newLogger failed")
		}
		bank, err := model.Isubank(tx)
		if err != nil {
			return errors.Wrap(err, "newIsubank failed")
		}
		amount, err := formvalInt64(r, "amount")
		if err != nil {
			return errcodeWrap(errors.Wrapf(err, "formvalInt64 failed. amount"), 400)
		}
		if amount <= 0 {
			return errcodeWrap(errors.Errorf("amount is must be greater 0. [%d]", amount), 400)
		}
		price, err := formvalInt64(r, "price")
		if err != nil {
			return errcodeWrap(errors.Wrapf(err, "formvalInt64 failed. price"), 400)
		}
		if price <= 0 {
			return errcodeWrap(errors.Errorf("price is must be greater 0. [%d]", price), 400)
		}
		ot := r.FormValue("type")
		switch ot {
		case model.OrderTypeBuy:
			totalPrice := price * amount
			if err = bank.Check(user.BankID, totalPrice); err != nil {
				le := logger.Send("buy.error", map[string]interface{}{
					"error":   err.Error(),
					"user_id": user.ID,
					"amount":  amount,
					"price":   price,
				})
				if le != nil {
					log.Printf("[WARN] logger.Send failed. err:%s", le)
				}
				if err == isubank.ErrCreditInsufficient {
					return errcode("銀行残高が足りません", 400)
				}
				return errors.Wrap(err, "isubank check failed")
			}
		case model.OrderTypeSell:
			// TODO 椅子の保有チェック
		default:
			return errcode("type must be sell or buy", 400)
		}
		res, err := tx.Exec(`INSERT INTO orders (type, user_id, amount, price, created_at) VALUES (?, ?, ?, ?, NOW(6))`, ot, user.ID, amount, price)
		if err != nil {
			return errors.Wrap(err, "insert order failed")
		}
		id, err = res.LastInsertId()
		if err != nil {
			return errors.Wrap(err, "get order_id failed")
		}
		le := logger.Send(ot+".order", map[string]interface{}{
			"order_id": id,
			"user_id":  user.ID,
			"amount":   amount,
			"price":    price,
		})
		if le != nil {
			log.Printf("[WARN] logger.Send failed. err:%s", le)
		}
		return nil
	})
	if err != nil {
		h.handleError(w, err, 500)
		return
	}

	tradeChance, err := model.HasTradeChanceByOrder(h.db, id)
	if err != nil {
		h.handleError(w, err, 500)
		return
	}
	if tradeChance {
		if err := model.RunTrade(h.db); err != nil {
			// トレードに失敗してもエラーにはしない
			log.Printf("runTrade err:%s", err)
		}
	}
	h.handleSuccess(w, map[string]interface{}{
		"id": id,
	})
}

func (h *Handler) GetOrders(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	user, err := h.userByRequest(r)
	if err != nil {
		h.handleError(w, err, 401)
		return
	}
	orders, err := model.GetOrdersByUserID(h.db, user.ID)
	if err != nil {
		h.handleError(w, err, 500)
		return
	}
	for _, order := range orders {
		if err = model.FetchOrderRelation(h.db, order); err != nil {
			h.handleError(w, err, 500)
			return
		}
	}
	h.handleSuccess(w, orders)
}

func (h *Handler) DeleteOrders(w http.ResponseWriter, r *http.Request, p httprouter.Params) {
	user, err := h.userByRequest(r)
	if err != nil {
		h.handleError(w, err, 401)
		return
	}
	var id int64
	err = txScorp(h.db, func(tx *sql.Tx) error {
		if _, err := model.GetUserByIDWithLock(tx, user.ID); err != nil {
			return errors.Wrapf(err, "model.GetUserByIDWithLock failed. id:%d", user.ID)
		}
		logger, err := model.Logger(tx)
		if err != nil {
			return errors.Wrap(err, "newLogger failed")
		}
		_id := p.ByName("id")
		if _id == "" {
			return errcodeWrap(errors.Errorf("id is required"), 400)
		}
		id, err = strconv.ParseInt(_id, 10, 64)
		if err != nil {
			return errcodeWrap(errors.Wrap(err, "id parse failed"), 400)
		}
		order, err := model.GetOrderByIDWithLock(tx, id)
		if err != nil {
			err = errors.Wrapf(err, "model.GetOrderByIDWithLock failed. id")
			if err == sql.ErrNoRows {
				return errcodeWrap(err, 404)
			}
			return err
		}
		if order.UserID != user.ID {
			return errcodeWrap(errors.New("not found"), 404)
		}
		if order.ClosedAt != nil {
			return errcodeWrap(errors.New("already closed"), 404)
		}
		if _, err = tx.Exec(`UPDATE orders SET closed_at = ? WHERE id = ?`, time.Now(), order.ID); err != nil {
			return errors.Wrap(err, "update orders for cancel")
		}
		le := logger.Send(order.Type+".delete", map[string]interface{}{
			"order_id": order.ID,
			"user_id":  order.UserID,
			"reason":   "canceled",
		})
		if le != nil {
			log.Printf("[WARN] logger.Send failed. err:%s", le)
		}
		return nil
	})

	if err != nil {
		h.handleError(w, err, 500)
		return
	}

	h.handleSuccess(w, map[string]interface{}{
		"id": id,
	})
}

func (h *Handler) commonHandler(f http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			if err := r.ParseForm(); err != nil {
				h.handleError(w, err, 400)
				return
			}
		}
		session, err := h.store.Get(r, SessionName)
		if err != nil {
			h.handleError(w, err, 500)
			return
		}
		if _userID, ok := session.Values["user_id"]; ok {
			userID := _userID.(int64)
			user, err := model.GetUserByID(h.db, userID)
			if err != nil {
				h.handleError(w, err, 500)
				return
			}
			ctx := context.WithValue(r.Context(), "user_id", user.ID)
			f.ServeHTTP(w, r.WithContext(ctx))
		} else {
			f.ServeHTTP(w, r)
		}
	})
}

func (h *Handler) userByRequest(r *http.Request) (*model.User, error) {
	v := r.Context().Value("user_id")
	if id, ok := v.(int64); ok {
		return model.GetUserByID(h.db, id)
	}
	return nil, errors.New("Not authenticated")
}

func (h *Handler) handleSuccess(w http.ResponseWriter, data interface{}) {
	w.WriteHeader(200)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(data); err != nil {
		log.Printf("[WARN] write response json failed. %s", err)
	}
}

func (h *Handler) handleError(w http.ResponseWriter, err error, code int) {
	if e, ok := err.(*errWithCode); ok {
		code = e.StatusCode
		err = e.Err
	}
	w.WriteHeader(code)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	log.Printf("[WARN] err: %s", err.Error())
	data := map[string]interface{}{
		"code": code,
		"err":  err.Error(),
	}
	if err := json.NewEncoder(w).Encode(data); err != nil {
		log.Printf("[WARN] write error response json failed. %s", err)
	}
}

// helpers

func formvalInt64(r *http.Request, key string) (int64, error) {
	v := r.FormValue(key)
	if v == "" {
		return 0, errors.Errorf("%s is required", key)
	}
	i, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		log.Printf("[INFO] can't parse to int64 key:%s val:%s err:%s", key, v, err)
		return 0, errors.Errorf("%s can't parse to int64", key)
	}
	return i, nil
}

func txScorp(db *sql.DB, f func(*sql.Tx) error) (err error) {
	var tx *sql.Tx
	tx, err = db.Begin()
	if err != nil {
		return errors.Wrap(err, "begin transaction failed")
	}
	defer func() {
		if e := recover(); e != nil {
			tx.Rollback()
			err = errors.Errorf("panic in transaction: %s", e)
		} else if err != nil {
			tx.Rollback()
		} else {
			err = tx.Commit()
		}
	}()
	err = f(tx)
	return
}

// databases
