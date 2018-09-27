package bench

import (
	"context"
	"log"
	"math/rand"
	"strings"
	"sync"
	"time"

	"github.com/ken39arg/isucon2018-final/bench/taskworker"
	"github.com/pkg/errors"
)

const (
	OrderCap = 5
)

type Investor interface {
	// 初期処理を実行するTaskを返す
	Start() taskworker.Task

	// tickerで呼ばれる
	Next() taskworker.Task

	BankID() string
	Credit() int64
	Isu() int64
	IsSignin() bool
	IsStarted() bool
	Orders() []*Order

	LatestTradePrice() int64
	IsRetired() bool
}

type investorBase struct {
	c                *Client
	defcredit        int64
	credit           int64
	resvedCredit     int64
	defisu           int64
	isu              int64
	resvedIsu        int64
	orders           []*Order
	lowestSellPrice  int64
	highestBuyPrice  int64
	latestTradePrice int64
	isSignin         bool
	isStarted        bool
	lastCursor       int64
	pollingTime      time.Time
	pollingLock      sync.Mutex
	lastOrder        time.Time
	actionLock       sync.Mutex
	taskLock         sync.Mutex
	taskStack        []taskworker.Task
	timeoutCount     int
}

func newInvestorBase(c *Client, credit, isu int64) *investorBase {
	orderMax := int(BenchMarkTime/OrderUpdateInterval) + 1
	return &investorBase{
		c:         c,
		defcredit: credit,
		credit:    credit,
		defisu:    isu,
		isu:       isu,
		orders:    make([]*Order, 0, orderMax),
		taskStack: make([]taskworker.Task, 0, 5),
	}
}

func (i *investorBase) IsRetired() bool {
	return i.c.IsRetired()
}

func (i *investorBase) LatestTradePrice() int64 {
	return i.latestTradePrice
}

func (i *investorBase) pushNextTask(task taskworker.Task) {
	i.taskLock.Lock()
	defer i.taskLock.Unlock()
	i.taskStack = append(i.taskStack, task)
}

func (i *investorBase) BankID() string {
	return i.c.bankid
}

func (i *investorBase) Credit() int64 {
	return i.credit
}

func (i *investorBase) Isu() int64 {
	return i.isu
}

func (i *investorBase) IsSignin() bool {
	return i.isSignin
}

func (i *investorBase) IsStarted() bool {
	return i.isStarted
}

func (i *investorBase) Orders() []*Order {
	return i.orders
}

func (i *investorBase) Top() taskworker.Task {
	return taskworker.NewExecTask(func(_ context.Context) error {
		return i.c.Top()
	}, GetTopScore)
}

func (i *investorBase) Signup() taskworker.Task {
	return taskworker.NewScoreTask(func(_ context.Context) (int64, error) {
		time.Sleep(time.Millisecond * time.Duration(rand.Int63n(100)))
		i.actionLock.Lock()
		defer i.actionLock.Unlock()
		if i.IsSignin() {
			return 0, nil
		}
		if err := i.c.Signup(); err != nil {
			if strings.Index(err.Error(), "bank_id already exists") > -1 {
				return SignupScore, nil
			}
			return 0, err
		}
		return SignupScore, nil
	})
}

func (i *investorBase) Signin() taskworker.Task {
	return taskworker.NewExecTask(func(_ context.Context) error {
		time.Sleep(time.Millisecond * time.Duration(rand.Int63n(100)))
		if err := i.c.Signin(); err != nil {
			return err
		}
		i.isSignin = true
		return nil

	}, SigninScore)
}

func (i *investorBase) Info() taskworker.Task {
	return taskworker.NewScoreTask(func(ctx context.Context) (int64, error) {
		i.pollingLock.Lock()
		defer i.pollingLock.Unlock()
		if i.IsRetired() {
			return 0, nil
		}
		now := time.Now()
		if now.Before(i.pollingTime) {
			//log.Printf("[INFO] skip info() next: %s now: %s", i.pollingTime, now)
			return 0, nil
		}
		info, err := i.c.Info(i.lastCursor)
		if err != nil {
			return 0, err
		}
		i.pollingTime = time.Now().Add(PollingInterval)
		i.lowestSellPrice = info.LowestSellPrice
		i.highestBuyPrice = info.HighestBuyPrice
		i.lastCursor = info.Cursor
		if l := len(info.ChartByHour); l > 0 {
			i.latestTradePrice = info.ChartByHour[l-1].Close
		}

		if info.TradedOrders != nil && len(info.TradedOrders) > 0 {
			// TODO 即実行のほうが良いか
			i.pushNextTask(i.UpdateOrders())
		}

		return GetInfoScore, nil
	})
}

func (i *investorBase) UpdateOrders() taskworker.Task {
	return taskworker.NewExecTask(func(_ context.Context) error {
		i.actionLock.Lock()
		defer i.actionLock.Unlock()
		orders, err := i.c.GetOrders()
		if err != nil {
			return err
		}
		if len(i.orders) > 0 {
			lo := i.orders[len(i.orders)-1]
			if lo.Type == TradeTypeSell {
				// 買い注文は即cancelされる可能性があるので調べない
				glo := orders[len(orders)-1]
				if lo.ID != glo.ID {
					return errors.Errorf("GET /orders 注文内容が反映されていません")
				}
			}
		}

		var resvedCredit, resvedIsu, tradedIsu, tradedCredit int64
		for _, o := range i.orders {
			if o.Removed() {
				continue
			}
			var order *Order
			for _, ro := range orders {
				if ro.ID == o.ID {
					order = &ro
					break
				}
			}
			if order == nil {
				// 自動的に消されたもの
				if o.Type == TradeTypeSell {
					return errors.Errorf("GET /orders 売り注文が足りないか削除されています %d", o.ID)
				}
				ct := time.Now()
				o.ClosedAt = &ct
				continue
			}
			*o = *order
			switch {
			case order.Trade != nil && order.Type == TradeTypeSell:
				// 成立済み 売り注文
				tradedIsu -= order.Amount
				tradedCredit += order.Amount * order.Trade.Price
			case order.Trade != nil && order.Type == TradeTypeBuy:
				// 成立済み 買い注文
				tradedIsu += order.Amount
				tradedCredit -= order.Amount * order.Trade.Price
			case order.Type == TradeTypeSell:
				// 売り注文
				resvedIsu += order.Amount
			case order.Type == TradeTypeBuy:
				// 買い注文
				resvedCredit += order.Amount * order.Price
			}
		}
		i.resvedIsu = resvedIsu
		i.resvedCredit = resvedCredit
		i.credit = i.defcredit + tradedCredit
		i.isu = i.defisu + tradedIsu
		return nil
	}, GetOrdersScore)
}

func (i *investorBase) AddOrder(ot string, amount, price int64) taskworker.Task {
	return taskworker.NewExecTask(func(ctx context.Context) error {
		i.actionLock.Lock()
		defer i.actionLock.Unlock()
		order, err := i.c.AddOrder(ot, amount, price)
		if err != nil {
			// 残高不足はOKとする
			if strings.Index(err.Error(), "銀行残高が足りません") > -1 {
				return nil
			}
			return err
		}
		i.orders = append(i.orders, order)
		i.lastOrder = time.Now()
		return nil
	}, PostOrdersScore)
}

func (i *investorBase) RemoveOrder(order *Order) taskworker.Task {
	return taskworker.NewScoreTask(func(ctx context.Context) (int64, error) {
		i.actionLock.Lock()
		defer i.actionLock.Unlock()
		if order.Removed() {
			return 0, nil
		}
		if err := i.c.DeleteOrders(order.ID); err != nil {
			if er, ok := err.(*ErrorWithStatus); ok && er.StatusCode == 404 {
				// 404エラーはしょうがないのでerrにはしないが加点しない
				log.Printf("[INFO] delete 404 %s", er)
				return 0, nil
			}
			return 0, err
		}
		found := false
		for _, o := range i.orders {
			if o.ID == order.ID {
				ct := time.Now()
				o.ClosedAt = &ct
				found = true
				break
			}
		}
		if !found {
			log.Printf("[WARN] not found removed order. %d", order.ID)
		}
		return DeleteOrdersScore, nil
	})
}

func (i *investorBase) Start() taskworker.Task {
	i.isStarted = true
	task := taskworker.NewSerialTask(6)
	task.Add(i.Top())
	task.Add(i.Info())
	task.Add(i.Signup())
	task.Add(i.Signin())
	task.Add(i.UpdateOrders())
	return task
}

func (i *investorBase) Next() taskworker.Task {
	i.taskLock.Lock()
	defer i.taskLock.Unlock()
	if i.IsRetired() {
		return nil
	}
	task := taskworker.NewSerialTask(2 + len(i.taskStack))
	task.Add(i.Info())
	for _, t := range i.taskStack {
		task.Add(t)
	}
	i.taskStack = i.taskStack[:0]
	return task
}

// あまり考えずに売買する
// 特徴:
//  - isuがunitamount以上余っていたら売りたい
//  - unitpriceよりも高値で売れそうなら売りたい
//  - unitpriceよりも安値で買えそうならunitamountの範囲で買いたい
//  - 取引価格がunitpriceとかけ離れたら深く考えずにunitpriceを見直す
type RandomInvestor struct {
	*investorBase
	unitamount int64
	unitprice  int64
}

func NewRandomInvestor(c *Client, credit, isu, unitamount, unitprice int64) *RandomInvestor {
	return &RandomInvestor{
		investorBase: newInvestorBase(c, credit, isu),
		unitamount:   unitamount,
		unitprice:    unitprice,
	}
}

func (i *RandomInvestor) Start() taskworker.Task {
	task := i.investorBase.Start()
	if t, ok := task.(*taskworker.SerialTask); ok {
		t.Add(i.FixNextTask())
		return t
	}
	return task
}

func (i *RandomInvestor) Next() taskworker.Task {
	if i.IsRetired() {
		return nil
	}
	task := i.investorBase.Next()
	if t, ok := task.(*taskworker.SerialTask); ok {
		t.Add(i.FixNextTask())
		return t
	}
	return task
}

func (i *RandomInvestor) FixNextTask() taskworker.Task {
	return taskworker.NewExecTask(func(_ context.Context) error {
		if task := i.UpdateOrderTask(); task != nil {
			i.pushNextTask(task)
			i.pushNextTask(i.UpdateOrders())
		}
		return nil
	}, 0)
}

func (i *RandomInvestor) UpdateOrderTask() taskworker.Task {
	i.actionLock.Lock()
	defer i.actionLock.Unlock()
	if i.IsRetired() {
		return nil
	}
	now := time.Now()
	update := len(i.orders) == 0 || i.lastOrder.Add(OrderUpdateInterval).After(now)

	if !update {
		return nil
	}
	logicalCredit := i.credit - i.resvedCredit
	logicalIsu := i.isu - i.resvedIsu
	switch {
	case len(i.orders) >= OrderCap:
		// orderを一個リリースする
		var o *Order
		var df int64
		for _, order := range i.orders {
			var mdiff int64
			if order.Type == TradeTypeSell {
				mdiff = order.Price - i.highestBuyPrice
			} else {
				mdiff = i.lowestSellPrice - order.Price
			}
			if o == nil || df < mdiff {
				o = order
				df = mdiff
			}
		}
		return i.RemoveOrder(o)
	case len(i.orders) == 0 && logicalIsu > i.unitamount:
		// 初注文は絶対する
		return i.AddOrder(TradeTypeSell, i.unitamount, i.unitprice)
	case len(i.orders) == 0:
		// 初注文は絶対する
		return i.AddOrder(TradeTypeBuy, i.unitamount, i.unitprice)
	case i.lowestSellPrice > 0 && i.lowestSellPrice < i.unitprice && i.lowestSellPrice <= logicalCredit:
		// 最安売値が設定値より安いので買いたい
		amount := rand.Int63n(logicalCredit/i.lowestSellPrice) + 1
		if i.unitamount < amount {
			amount = i.unitamount
		}
		return i.AddOrder(TradeTypeBuy, amount, i.lowestSellPrice)
	case i.highestBuyPrice > 0 && i.highestBuyPrice > i.unitprice && logicalIsu > 0:
		// 最高買値が設定値より高いので売りたい
		amount := rand.Int63n(logicalIsu) + 1
		if i.unitamount < amount {
			amount = i.unitamount
		}
		return i.AddOrder(TradeTypeSell, amount, i.highestBuyPrice)
	case logicalIsu > i.unitamount:
		// 椅子をたくさん持っていて現在価格が希望外のときは少し妥協して売りに行く
		price := (i.lowestSellPrice + i.unitprice) / 2
		if i.lowestSellPrice == 0 {
			price = i.unitprice
		}
		amount := rand.Int63n(i.unitamount) + 1
		return i.AddOrder(TradeTypeSell, amount, price)
	case logicalCredit > (i.highestBuyPrice+i.unitprice)/2:
		// 金があるので妥協価格で買い注文を入れる
		price := (i.highestBuyPrice + i.unitprice) / 2
		if i.highestBuyPrice == 0 {
			price = i.unitprice
		}
		amount := rand.Int63n(i.unitamount) + 1
		return i.AddOrder(TradeTypeBuy, amount, price)
	default:
		// 椅子評価額の見直し
		if latestPrice := i.LatestTradePrice(); latestPrice > 0 {
			i.unitprice = (latestPrice + i.unitprice) / 2
		}
	}
	return nil
}
