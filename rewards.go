package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// AutoClaw pays out in two places the desktop client reaches on its own:
//
//	POST /userapi/v1/user-info                      the "login active" hook
//	GET  /autoclaw-proxy/proxy/autoclaw-task-list    daily task board
//	POST /autoclaw-proxy/proxy/autoclaw-task-complete {task_id}
//
// The login reward (campaign credits, e.g. +4000) is not a claim endpoint: the
// server grants it when the client announces a login through user-info, which
// the desktop does on every start. A proxy that only refreshes tokens never
// triggers it, so the pool silently misses those credits. The daily tasks are
// explicit: the board says what is left today and task-complete pays it out.
const (
	userInfoPath     = "/userapi/v1/user-info"
	taskListPath     = "/autoclaw-proxy/proxy/autoclaw-task-list"
	taskCompletePath = "/autoclaw-proxy/proxy/autoclaw-task-complete"

	// Campaign ledger rows the login hook produces; both prefixes exist in the
	// wild (daily perks and the one-off new-user pack).
	campaignEventActive = "autoclaw_user_active"
	campaignEventSignup = "autoclaw_user_signup_day"
	campaignActivityA   = "autoclaw_user_active_reward"
	campaignActivityB   = "autoclaw_user_active_bonus"
	campaignActivityC   = "autoclaw_new_user_day_1_to_7_bonus"
)

// rewardTask is one row of the task board.
type rewardTask struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Type        string `json:"type"`
	Reward      int64  `json:"reward"`
	Status      string `json:"status"`
	Description string `json:"description,omitempty"`
	Display     bool   `json:"display"`
	ClientSide  bool   `json:"client_triggered"`
	Done        bool   `json:"done"`
}

// rewardClaim is the outcome of one task-complete call.
type rewardClaim struct {
	TaskID  string `json:"task_id"`
	Title   string `json:"title,omitempty"`
	Points  int64  `json:"points"`
	Claimed bool   `json:"claimed"`
	Already bool   `json:"already,omitempty"`
	Message string `json:"message,omitempty"`
}

// rewardResult is what the console shows after a claim run.
type rewardResult struct {
	Account        string        `json:"account,omitempty"`
	LoginTriggered bool          `json:"login_triggered"`
	LoginPoints    int64         `json:"login_points"`
	LoginGain      int64         `json:"login_gain"`
	LoginEntries   []string      `json:"login_entries,omitempty"`
	Claims         []rewardClaim `json:"claims,omitempty"`
	Points         int64         `json:"points"`
	Message        string        `json:"message"`
	At             int64         `json:"at"`
	Error          string        `json:"error,omitempty"`
}

// rewardLock serialises reward runs per account: the login hook, the background
// loop and the console can all fire at once, and duplicate task-complete calls
// would race on RewardDate.
func (a *app) rewardLock(id string) *sync.Mutex {
	a.rewardMu.Lock()
	defer a.rewardMu.Unlock()
	if a.rewardLocks == nil {
		a.rewardLocks = map[string]*sync.Mutex{}
	}
	lock, ok := a.rewardLocks[id]
	if !ok {
		lock = &sync.Mutex{}
		a.rewardLocks[id] = lock
	}
	return lock
}

func beijingNow() time.Time {
	return time.Now().In(time.FixedZone("CST", 8*3600))
}

func beijingDate() string {
	return beijingNow().Format("2006-01-02")
}

// identityCall posts/gets one identity-gateway path and returns the decoded
// envelope data, surfacing the upstream code so failures stay diagnosable.
func (a *app) identityCall(ctx context.Context, account *credential, method, path string, body any) (json.RawMessage, error) {
	cfg := a.currentConfig()
	if account == nil || account.AccessToken == "" {
		return nil, fmt.Errorf("账号没有 access_token")
	}
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, cfg.AuthBaseURL+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header = a.creditHeaders(cfg, account)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求 AutoClaw 失败：%w", err)
	}
	defer resp.Body.Close()
	raw, err := readLimited(resp.Body, 1<<20)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("AutoClaw 返回 HTTP %d：%s", resp.StatusCode, truncateRunes(string(raw), 160))
	}
	var envelope struct {
		Code json.Number     `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("响应不是 JSON：%s", truncateRunes(string(raw), 160))
	}
	if code := envelope.Code.String(); code != "" && code != "0" {
		return nil, fmt.Errorf("%s（code=%s）", firstNonEmpty(strings.TrimSpace(envelope.Msg), "上游拒绝请求"), code)
	}
	return envelope.Data, nil
}

// triggerLoginActive replays the client's startup call. This is the hook that
// makes AutoClaw hand out login campaign credits.
func (a *app) triggerLoginActive(ctx context.Context, account *credential) error {
	_, err := a.identityCall(ctx, account, http.MethodPost, userInfoPath, map[string]any{
		"source_id": "autoclaw",
		"device_id": a.accountDevice(a.currentConfig(), account),
	})
	return err
}

func (a *app) fetchTasks(ctx context.Context, account *credential) ([]rewardTask, error) {
	data, err := a.identityCall(ctx, account, http.MethodGet, taskListPath, nil)
	if err != nil {
		return nil, err
	}
	var rows []struct {
		ID          string `json:"task_id"`
		Title       string `json:"title"`
		Type        string `json:"task_type"`
		Reward      int64  `json:"reward_points"`
		Status      string `json:"status"`
		Description string `json:"point_description"`
		Display     bool   `json:"display"`
		ClientSide  bool   `json:"client_triggered"`
	}
	if err := json.Unmarshal(data, &rows); err != nil {
		return nil, fmt.Errorf("任务列表格式不对：%s", truncateRunes(string(data), 160))
	}
	tasks := make([]rewardTask, 0, len(rows))
	for _, row := range rows {
		status := strings.ToLower(strings.TrimSpace(row.Status))
		tasks = append(tasks, rewardTask{
			ID:          strings.TrimSpace(row.ID),
			Title:       strings.TrimSpace(row.Title),
			Type:        strings.TrimSpace(row.Type),
			Reward:      row.Reward,
			Status:      status,
			Description: strings.TrimSpace(row.Description),
			Display:     row.Display,
			ClientSide:  row.ClientSide,
			Done:        status == "complete" || status == "completed" || status == "claimed",
		})
	}
	return tasks, nil
}

func (a *app) completeTask(ctx context.Context, account *credential, taskID string) (rewardClaim, error) {
	claim := rewardClaim{TaskID: taskID}
	data, err := a.identityCall(ctx, account, http.MethodPost, taskCompletePath, map[string]any{"task_id": taskID})
	if err != nil {
		claim.Message = err.Error()
		return claim, err
	}
	var payload struct {
		Success          bool   `json:"success"`
		AlreadyCompleted bool   `json:"already_completed"`
		RewardPoints     int64  `json:"reward_points"`
		TaskID           string `json:"task_id"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		claim.Message = "任务响应格式不对：" + truncateRunes(string(data), 120)
		return claim, fmt.Errorf("%s", claim.Message)
	}
	claim.Points = payload.RewardPoints
	claim.Already = payload.AlreadyCompleted
	// A repeat call answers success:false + already_completed:true, which is not
	// a fresh payout; only points (or a real success) count as claimed.
	claim.Claimed = payload.RewardPoints > 0 || (payload.Success && !payload.AlreadyCompleted)
	switch {
	case claim.Claimed:
		claim.Message = fmt.Sprintf("+%d 积分", payload.RewardPoints)
	case payload.AlreadyCompleted:
		claim.Message = "今天已经领过"
	default:
		claim.Message = "上游没有发放积分"
	}
	return claim, nil
}

// todayCampaignPoints lists the login-reward rows the hook produced today.
func (a *app) todayCampaignPoints(ctx context.Context, account *credential) (int64, []string) {
	entries, err := a.fetchLedgersWith(ctx, account, 50, "income")
	if err != nil {
		return 0, nil
	}
	today := beijingDate()
	var total int64
	var lines []string
	for _, entry := range entries {
		if entry.Amount <= 0 {
			continue
		}
		if !campaignEntry(entry) {
			continue
		}
		// The row's own timestamp is authoritative; grant_date is the campaign
		// period label and can lag the payout by days.
		if date := firstNonEmpty(ledgerDate(entry.At), entry.GrantDate); date != "" && date != today {
			continue
		}
		total += entry.Amount
		lines = append(lines, fmt.Sprintf("%s +%d", firstNonEmpty(entry.Description, "活动奖励"), entry.Amount))
	}
	return total, lines
}

func campaignEntry(entry creditLedger) bool {
	event := strings.ToLower(strings.TrimSpace(entry.EventType))
	if event != "" {
		return strings.HasPrefix(event, campaignEventActive) || strings.HasPrefix(event, campaignEventSignup)
	}
	reason := strings.ToLower(strings.TrimSpace(entry.Description + " " + entry.Reason))
	return strings.Contains(reason, campaignActivityA) ||
		strings.Contains(reason, campaignActivityB) ||
		strings.Contains(reason, campaignActivityC)
}

func ledgerDate(at string) string {
	parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(at))
	if err != nil {
		return ""
	}
	return parsed.In(time.FixedZone("CST", 8*3600)).Format("2006-01-02")
}

// claimRewards triggers the login hook, then pays out every daily task that is
// still open. taskID limits the run to one task.
func (a *app) claimRewards(ctx context.Context, account *credential, taskID string) rewardResult {
	result := rewardResult{At: time.Now().UnixMilli()}
	if account == nil {
		result.Error = "账号不存在"
		result.Message = result.Error
		return result
	}
	result.Account = account.Alias

	if fresh, errFresh := a.ensureAccountFresh(ctx, account); errFresh == nil && fresh != nil {
		account = fresh
	}
	lock := a.rewardLock(account.ID)
	lock.Lock()
	defer lock.Unlock()

	if taskID == "" {
		before, _ := a.todayCampaignPoints(ctx, account)
		if err := a.triggerLoginActive(ctx, account); err != nil {
			result.Error = err.Error()
			a.log.warn("rewards", fmt.Sprintf("账号 %s 触发登录奖励失败：%v", account.Alias, err))
		} else {
			result.LoginTriggered = true
			// The payout lands a beat after the call; give it a moment before
			// reading the ledger, and never fail the run when it is late.
			select {
			case <-ctx.Done():
			case <-time.After(1500 * time.Millisecond):
			}
			result.LoginPoints, result.LoginEntries = a.todayCampaignPoints(ctx, account)
			if gain := result.LoginPoints - before; gain > 0 {
				result.LoginGain = gain
			}
		}
	}

	tasks, err := a.fetchTasks(ctx, account)
	if err != nil {
		if result.Error == "" {
			result.Error = err.Error()
		}
		result.Message = firstNonEmpty(result.Error, "任务列表读取失败")
		return result
	}
	for _, task := range tasks {
		if task.ID == "" || task.Done || task.Reward <= 0 {
			continue
		}
		if taskID != "" && task.ID != taskID {
			continue
		}
		if taskID == "" && !claimableTask(task) {
			continue
		}
		claim, errClaim := a.completeTask(ctx, account, task.ID)
		claim.Title = task.Title
		if errClaim != nil && claim.Message == "" {
			claim.Message = errClaim.Error()
		}
		if claim.Claimed {
			result.Points += claim.Points
		}
		result.Claims = append(result.Claims, claim)
	}

	switch {
	case result.Points > 0:
		result.Message = fmt.Sprintf("领取成功 +%d 积分", result.Points)
	case result.LoginGain > 0:
		result.Message = fmt.Sprintf("登录奖励到账 +%d 积分", result.LoginGain)
	case result.LoginPoints > 0:
		result.Message = fmt.Sprintf("今日登录奖励已到账 +%d 积分（本次无新增）", result.LoginPoints)
	default:
		result.Message = "今天没有可领取的积分"
	}
	if len(result.Claims) == 0 && taskID != "" {
		result.Message = "这个任务今天已经完成"
	}

	// Balances just changed; make the next read show it.
	a.dropCreditsCache(account.ID)
	// Only a full run that actually paid out closes the day: a failed call, an
	// expired context or a single console task must not stop the background loop
	// from retrying later.
	if taskID == "" && (result.Points > 0 || result.LoginPoints > 0) {
		today := beijingDate()
		account.RewardDate = today
		a.touchAccount(account.ID, func(current *credential) {
			current.RewardDate = today
		})
	}
	return result
}

// claimableTask is the subset a proxy can safely claim on its own: the daily
// board tasks the client itself triggers. Invite-style tasks need a real user.
func claimableTask(task rewardTask) bool {
	if task.Reward <= 0 || task.Done {
		return false
	}
	if strings.EqualFold(task.Type, "daily") {
		return true
	}
	return task.ClientSide && strings.HasPrefix(task.ID, "daily_")
}

func (a *app) claimRewardsQuietly(account *credential, onlyLogin bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	result := a.claimRewards(ctx, account, "")
	if onlyLogin {
		a.log.info("rewards", fmt.Sprintf("账号 %s 登录奖励：%s", result.Account, result.Message))
		return
	}
	a.log.info("rewards", fmt.Sprintf("账号 %s 每日积分：%s", result.Account, result.Message))
}

// rewardLoop keeps the pool's daily credits flowing without the console open.
func (a *app) rewardLoop(done <-chan struct{}) {
	interval := time.Duration(a.currentConfig().RewardIntervalMinutes) * time.Minute
	if interval <= 0 {
		interval = 30 * time.Minute
	}
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		select {
		case <-done:
			return
		case <-timer.C:
			interval = time.Duration(a.currentConfig().RewardIntervalMinutes) * time.Minute
			if interval <= 0 {
				interval = 30 * time.Minute
			}
			timer.Reset(interval)
			cfg := a.currentConfig()
			if !cfg.DailyReward {
				continue
			}
			today := beijingDate()
			for _, account := range a.poolSnapshot() {
				if account.Disabled || account.Dead || account.AccessToken == "" {
					continue
				}
				if account.RewardDate == today {
					continue
				}
				a.claimRewardsQuietly(account, false)
			}
		}
	}
}

func (a *app) touchAccount(id string, mutate func(*credential)) {
	if id == "" || mutate == nil {
		return
	}
	a.credMu.Lock()
	account, ok := a.accounts[id]
	if ok {
		mutate(account)
	}
	a.credMu.Unlock()
	if ok {
		a.savePool()
	}
}

// pickAccount resolves a console selection to a pool account, defaulting to the
// first one so the buttons work without an explicit choice.
func (a *app) pickAccount(idOrAlias string) *credential {
	accounts := a.poolSnapshot()
	key := strings.TrimSpace(idOrAlias)
	if key != "" {
		for _, account := range accounts {
			if account.ID == key || account.Alias == key {
				return account
			}
		}
	}
	if len(accounts) == 0 {
		return nil
	}
	for _, account := range accounts {
		if !account.Disabled && !account.Dead && account.LastError == "" {
			return account
		}
	}
	for _, account := range accounts {
		if !account.Disabled && !account.Dead {
			return account
		}
	}
	return accounts[0]
}

// tasksPayload is the 积分 tab's task board plus today's login-reward status.
func (a *app) tasksPayload(accountID string) map[string]any {
	target := a.pickAccount(accountID)
	if target == nil {
		return map[string]any{"ok": false, "message": "账号池是空的", "tasks": []rewardTask{}}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tasks, err := a.fetchTasks(ctx, target)
	if err != nil {
		if accountGone(err) {
			a.markAccountDead(target.ID, err.Error())
		}
		return map[string]any{"ok": false, "message": err.Error(), "account": target.Alias, "account_id": target.ID, "tasks": []rewardTask{}}
	}
	points, entries := a.todayCampaignPoints(ctx, target)
	claimable := 0
	var claimablePoints int64
	for _, task := range tasks {
		if claimableTask(task) {
			claimable++
			claimablePoints += task.Reward
		}
	}
	return map[string]any{
		"ok":               true,
		"account":          target.Alias,
		"account_id":       target.ID,
		"tasks":            tasks,
		"claimable":        claimable,
		"claimable_points": claimablePoints,
		"today_points":     points,
		"today_entries":    entries,
		"reward_date":      target.RewardDate,
	}
}
