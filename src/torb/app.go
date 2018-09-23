package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	_ "expvar"
	"fmt"
	"html/template"
	"io"
	"log"
	_ "net/http/pprof"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/gorilla/sessions"
	"github.com/labstack/echo"
	"github.com/labstack/echo-contrib/session"
	"github.com/labstack/echo/middleware"
	"github.com/sevenNt/echo-pprof"
)

type User struct {
	ID        int64  `json:"id,omitempty"`
	Nickname  string `json:"nickname,omitempty"`
	LoginName string `json:"login_name,omitempty"`
	PassHash  string `json:"pass_hash,omitempty"`
}

type Event struct {
	ID       int64  `json:"id,omitempty"`
	Title    string `json:"title,omitempty"`
	PublicFg bool   `json:"public,omitempty"`
	ClosedFg bool   `json:"closed,omitempty"`
	Price    int64  `json:"price,omitempty"`

	Total   int                `json:"total"`
	Remains int                `json:"remains"`
	Sheets  map[string]*Sheets `json:"sheets,omitempty"`
}

type Sheets struct {
	Total   int      `json:"total"`
	Remains int      `json:"remains"`
	Detail  []*Sheet `json:"detail,omitempty"`
	Price   int64    `json:"price"`
}

type Sheet struct {
	ID    int64  `json:"-"`
	Rank  string `json:"-"`
	Num   int64  `json:"num"`
	Price int64  `json:"-"`

	Mine           bool       `json:"mine,omitempty"`
	Reserved       bool       `json:"reserved,omitempty"`
	ReservedAt     *time.Time `json:"-"`
	ReservedAtUnix int64      `json:"reserved_at,omitempty"`
}

type Reservation struct {
	ID         int64      `json:"id"`
	EventID    int64      `json:"-"`
	SheetID    int64      `json:"-"`
	UserID     int64      `json:"-"`
	ReservedAt *time.Time `json:"-"`
	CanceledAt *time.Time `json:"-"`

	Event          *Event `json:"event,omitempty"`
	SheetRank      string `json:"sheet_rank,omitempty"`
	SheetNum       int64  `json:"sheet_num,omitempty"`
	Price          int64  `json:"price,omitempty"`
	ReservedAtUnix int64  `json:"reserved_at,omitempty"`
	CanceledAtUnix int64  `json:"canceled_at,omitempty"`
}

type Administrator struct {
	ID        int64  `json:"id,omitempty"`
	Nickname  string `json:"nickname,omitempty"`
	LoginName string `json:"login_name,omitempty"`
	PassHash  string `json:"pass_hash,omitempty"`
}

var (
	eventPrice map[int64]int64
)

func sessUserID(c echo.Context) int64 {
	sess, _ := session.Get("session", c)
	var userID int64
	if x, ok := sess.Values["user_id"]; ok {
		userID, _ = x.(int64)
	}
	return userID
}

func sessSetUserID(c echo.Context, id int64) {
	sess, _ := session.Get("session", c)
	sess.Options = &sessions.Options{
		Path:     "/",
		MaxAge:   3600,
		HttpOnly: true,
	}
	sess.Values["user_id"] = id
	sess.Save(c.Request(), c.Response())
}

func sessDeleteUserID(c echo.Context) {
	sess, _ := session.Get("session", c)
	sess.Options = &sessions.Options{
		Path:     "/",
		MaxAge:   3600,
		HttpOnly: true,
	}
	delete(sess.Values, "user_id")
	sess.Save(c.Request(), c.Response())
}

func sessAdministratorID(c echo.Context) int64 {
	sess, _ := session.Get("session", c)
	var administratorID int64
	if x, ok := sess.Values["administrator_id"]; ok {
		administratorID, _ = x.(int64)
	}
	return administratorID
}

func sessSetAdministratorID(c echo.Context, id int64) {
	sess, _ := session.Get("session", c)
	sess.Options = &sessions.Options{
		Path:     "/",
		MaxAge:   3600,
		HttpOnly: true,
	}
	sess.Values["administrator_id"] = id
	sess.Save(c.Request(), c.Response())
}

func sessDeleteAdministratorID(c echo.Context) {
	sess, _ := session.Get("session", c)
	sess.Options = &sessions.Options{
		Path:     "/",
		MaxAge:   3600,
		HttpOnly: true,
	}
	delete(sess.Values, "administrator_id")
	sess.Save(c.Request(), c.Response())
}

func loginRequired(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		if _, err := getLoginUser(c); err != nil {
			return resError(c, "login_required", 401)
		}
		return next(c)
	}
}

func adminLoginRequired(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		if _, err := getLoginAdministrator(c); err != nil {
			return resError(c, "admin_login_required", 401)
		}
		return next(c)
	}
}

func getLoginUser(c echo.Context) (*User, error) {
	userID := sessUserID(c)
	if userID == 0 {
		return nil, errors.New("not logged in")
	}
	var user User
	err := db.QueryRow("SELECT id, nickname FROM users WHERE id = ?", userID).Scan(&user.ID, &user.Nickname)
	return &user, err
}

func getLoginAdministrator(c echo.Context) (*Administrator, error) {
	administratorID := sessAdministratorID(c)
	if administratorID == 0 {
		return nil, errors.New("not logged in")
	}
	administrator, ok := id2admin[administratorID]
	if !ok {
		return nil, fmt.Errorf("ADMINISTRATOR NOT FOUND (ID: %d)\n", administratorID)
	}
	return &administrator, nil
}

func getEvents(all bool) ([]*Event, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Commit()

	rows, err := tx.Query("SELECT * FROM events ORDER BY id ASC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []*Event
	for rows.Next() {
		var event Event
		if err := rows.Scan(&event.ID, &event.Title, &event.PublicFg, &event.ClosedFg, &event.Price); err != nil {
			return nil, err
		}
		if !all && !event.PublicFg {
			continue
		}
		events = append(events, &event)
	}
	for i, v := range events {
		event, err := getEventWithTransaction(v.ID, -1, tx)
		if err != nil {
			return nil, err
		}
		for k := range event.Sheets {
			event.Sheets[k].Detail = nil
		}
		events[i] = event
	}
	return events, nil
}

type ReservedSheet struct {
	userID     int64
	reservedAt *time.Time
}

func getSheetRankIndex(rank string) int {
	switch rank {
	case "C":
		return 3
	case "B":
		return 2
	case "A":
		return 1
	default:
		return 0
	}
}
func toMappedSheets(eventSheets []*Sheets) map[string]*Sheets {
	return map[string]*Sheets{
		"S": eventSheets[0],
		"A": eventSheets[1],
		"B": eventSheets[2],
		"C": eventSheets[3],
	}
}

func initSheets(price int64) []*Sheets {
	eventSheets := []*Sheets{
		&Sheets{
			Price:  price + 5000,
			Total:  50,
			Remains:50,
			Detail : make([]*Sheet,50),
		},
		&Sheets{
			Price:  price + 3000,
			Total:  150,
			Remains:150,
			Detail : make([]*Sheet,150),
		},
		&Sheets{
			Price:  price + 1000,
			Total:  300,
			Remains:300,
			Detail : make([]*Sheet,300),
		},
		&Sheets{
			Price:  price,
			Total:  500,
			Remains:500,
			Detail : make([]*Sheet,500),
		},
	}
	details := make([]Sheet,1000)
	copy(details,orderdSheets)
	for i := range orderdSheets {
		eventSheets[getRankIndexByIndex(i)].Detail[getDetailIndexByIndex(i)] = &details[i]
	}
	return eventSheets
}
func getRankIndexByIndex(i int) int {
	// A : [0 , 150)
	// B : [150, 450)
	// C : [450, 950)
	// S : [950, 1000)
	if i < 150 {
		return 1
	} else if i < 450 {
		return 2
	} else if i < 950 {
		return 3
	} else  {
		return 0
	}
}
func getDetailIndexByIndex(i int) int {
	if i < 150 {
		return i
	} else if i < 450 {
		return i - 150
	} else if i < 950 {
		return i - 450
	} else  {
		return i - 950
	}
}
func getSheetIdByIndex(i int) int {
	if i < 950 {
		return i + 51
	} else {
		return i - 949
	}
}
func getIndexBySheetId(sheetId int) int {
	if sheetId <= 50 {
		return sheetId + 949
	} else {
		return sheetId - 51
	}
}
func getEventImpl(eventID, loginUserID int64,tx *sql.Tx) (*Event, error) {
	var event Event
	var row *sql.Row
	sql1 := "SELECT * FROM events WHERE id = ?"
	if tx != nil {
		row = tx.QueryRow(sql1, eventID)
	} else {
		row = db.QueryRow(sql1, eventID)
	}
	if err := row.Scan(&event.ID, &event.Title, &event.PublicFg, &event.ClosedFg, &event.Price); err != nil {
		return nil, err
	}
	event.Total = 1000
	event.Remains = 1000
	var rows *sql.Rows
	var err error
	sql2 := "SELECT user_id, sheet_id, reserved_at FROM reservations WHERE event_id = ? AND canceled_at IS NULL GROUP BY sheet_id HAVING reserved_at = MIN(reserved_at)"
	if tx != nil {
		rows, err = tx.Query(sql2, eventID)
	} else {
		rows, err = db.Query(sql2, eventID)
	}
	if err == sql.ErrNoRows {
		event.Remains = 1000
		eventSheets := initSheets(event.Price)
		event.Sheets = toMappedSheets(eventSheets)
		return &event, nil
	} else if err != nil {
		return nil, err
	}
	defer rows.Close()

	eventSheets := initSheets(event.Price)
	for rows.Next() {
		var userID int64
		var sheetID int64
		var reservedAt *time.Time
		err := rows.Scan(&userID, &sheetID, &reservedAt)
		if err != nil {
			return nil, err
		}
		i := getIndexBySheetId(int(sheetID))
		rankIndex := getRankIndexByIndex(i)
		detailIndex := getDetailIndexByIndex(i)
		eventSheets[rankIndex].Detail[detailIndex].Mine = userID == loginUserID
		eventSheets[rankIndex].Detail[detailIndex].Reserved = true
		eventSheets[rankIndex].Detail[detailIndex].ReservedAtUnix = reservedAt.Unix()
		event.Remains--
		eventSheets[rankIndex].Remains--
	}
	event.Sheets = toMappedSheets(eventSheets)
	return &event, nil
}

func getEventWithTransaction(eventID, loginUserID int64, tx *sql.Tx) (*Event, error) {
	return getEventImpl(eventID, loginUserID, tx)
}
func getEvent(eventID, loginUserID int64) (*Event, error) {
	return getEventImpl(eventID, loginUserID, nil)
}

func sanitizeEvent(e *Event) *Event {
	sanitized := *e
	sanitized.Price = 0
	sanitized.PublicFg = false
	sanitized.ClosedFg = false
	return &sanitized
}

func fillinUser(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		if user, err := getLoginUser(c); err == nil {
			c.Set("user", user)
		}
		return next(c)
	}
}

func fillinAdministrator(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		if administrator, err := getLoginAdministrator(c); err == nil {
			c.Set("administrator", administrator)
		}
		return next(c)
	}
}

func validateRank(rank string) bool {
	var count int
	db.QueryRow("SELECT COUNT(*) FROM sheets WHERE `rank` = ?", rank).Scan(&count)
	return count > 0
}

type Renderer struct {
	templates *template.Template
}

func (r *Renderer) Render(w io.Writer, name string, data interface{}, c echo.Context) error {
	return r.templates.ExecuteTemplate(w, name, data)
}

var db *sql.DB

func encodeJson(v interface{}) string {
	b, _ := json.Marshal(v)
	return string(b)
}
func getIndex(c echo.Context) error {
	events, err := getEvents(false)
	if err != nil {
		return err
	}
	for i, v := range events {
		events[i] = sanitizeEvent(v)
	}
	return c.Render(200, "index.tmpl", echo.Map{
		"events": events,
		"user":   c.Get("user"),
		"origin": c.Scheme() + "://" + c.Request().Host,
	})
}
func getInitialize(c echo.Context) error {
	cmd := exec.Command("../../db/init.sh")
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	err := cmd.Run()
	if err != nil {
		return nil
	}

	// aokabi
	eventPrice = make(map[int64]int64)
	rows, err := db.Query("SELECT id, price FROM events")
	if err != nil {
		return err
	}

	for rows.Next() {
		var event Event
		if err := rows.Scan(&event.ID, &event.Price); err != nil {
			return err
		}
		eventPrice[event.ID] = event.Price
	}
	// end aokabi

	return c.NoContent(204)
}
func postUsers(c echo.Context) error {
	var params struct {
		Nickname  string `json:"nickname"`
		LoginName string `json:"login_name"`
		Password  string `json:"password"`
	}
	c.Bind(&params)

	tx, err := db.Begin()
	if err != nil {
		return err
	}

	var user User
	if err := tx.QueryRow("SELECT * FROM users WHERE login_name = ?", params.LoginName).Scan(&user.ID, &user.LoginName, &user.Nickname, &user.PassHash); err != sql.ErrNoRows {
		tx.Rollback()
		if err == nil {
			return resError(c, "duplicated", 409)
		}
		return err
	}

	res, err := tx.Exec("INSERT INTO users (login_name, pass_hash, nickname) VALUES (?, SHA2(?, 256), ?)", params.LoginName, params.Password, params.Nickname)
	if err != nil {
		tx.Rollback()
		return resError(c, "", 0)
	}
	userID, err := res.LastInsertId()
	if err != nil {
		tx.Rollback()
		return resError(c, "", 0)
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	return c.JSON(201, echo.Map{
		"id":       userID,
		"nickname": params.Nickname,
	})
}

func getUser(c echo.Context) error {
	var user User
	if err := db.QueryRow("SELECT id, nickname FROM users WHERE id = ?", c.Param("id")).Scan(&user.ID, &user.Nickname); err != nil {
		return err
	}

	loginUser, err := getLoginUser(c)
	if err != nil {
		return err
	}
	if user.ID != loginUser.ID {
		return resError(c, "forbidden", 403)
	}

	rows, err := db.Query("SELECT r.*, s.rank AS sheet_rank, s.num AS sheet_num FROM reservations r INNER JOIN sheets s ON s.id = r.sheet_id WHERE r.user_id = ? ORDER BY IFNULL(r.canceled_at, r.reserved_at) DESC LIMIT 5", user.ID)
	if err != nil {
		return err
	}
	defer rows.Close()

	var recentReservations []Reservation
	for rows.Next() {
		var reservation Reservation
		var sheet Sheet
		if err := rows.Scan(&reservation.ID, &reservation.EventID, &reservation.SheetID, &reservation.UserID, &reservation.ReservedAt, &reservation.CanceledAt, &sheet.Rank, &sheet.Num); err != nil {
			return err
		}

		event, err := getEvent(reservation.EventID, -1)
		if err != nil {
			return err
		}
		price := event.Sheets[sheet.Rank].Price
		event.Sheets = nil
		event.Total = 0
		event.Remains = 0

		reservation.Event = event
		reservation.SheetRank = sheet.Rank
		reservation.SheetNum = sheet.Num
		reservation.Price = price
		reservation.ReservedAtUnix = reservation.ReservedAt.Unix()
		if reservation.CanceledAt != nil {
			reservation.CanceledAtUnix = reservation.CanceledAt.Unix()
		}
		recentReservations = append(recentReservations, reservation)
	}
	if recentReservations == nil {
		recentReservations = make([]Reservation, 0)
	}

	var totalPrice int
	if err := db.QueryRow("SELECT IFNULL(SUM(e.price + s.price), 0) FROM reservations r INNER JOIN sheets s ON s.id = r.sheet_id INNER JOIN events e ON e.id = r.event_id WHERE r.user_id = ? AND r.canceled_at IS NULL", user.ID).Scan(&totalPrice); err != nil {
		return err
	}

	rows, err = db.Query("SELECT event_id FROM reservations WHERE user_id = ? GROUP BY event_id ORDER BY MAX(IFNULL(canceled_at, reserved_at)) DESC LIMIT 5", user.ID)
	if err != nil {
		return err
	}
	defer rows.Close()

	var recentEvents []*Event
	for rows.Next() {
		var eventID int64
		if err := rows.Scan(&eventID); err != nil {
			return err
		}
		event, err := getEvent(eventID, -1)
		if err != nil {
			return err
		}
		for k := range event.Sheets {
			event.Sheets[k].Detail = nil
		}
		recentEvents = append(recentEvents, event)
	}
	if recentEvents == nil {
		recentEvents = make([]*Event, 0)
	}

	return c.JSON(200, echo.Map{
		"id":                  user.ID,
		"nickname":            user.Nickname,
		"recent_reservations": recentReservations,
		"total_price":         totalPrice,
		"recent_events":       recentEvents,
	})
}

func postLogin(c echo.Context) error {
	var params struct {
		LoginName string `json:"login_name"`
		Password  string `json:"password"`
	}
	c.Bind(&params)

	user := new(User)
	if err := db.QueryRow("SELECT * FROM users WHERE login_name = ?", params.LoginName).Scan(&user.ID, &user.LoginName, &user.Nickname, &user.PassHash); err != nil {
		if err == sql.ErrNoRows {
			return resError(c, "authentication_failed", 401)
		}
		return err
	}

	var passHash string
	if err := db.QueryRow("SELECT SHA2(?, 256)", params.Password).Scan(&passHash); err != nil {
		return err
	}
	if user.PassHash != passHash {
		return resError(c, "authentication_failed", 401)
	}

	sessSetUserID(c, user.ID)
	user, err := getLoginUser(c)
	if err != nil {
		return err
	}
	return c.JSON(200, user)
}
func postLogout(c echo.Context) error {
	sessDeleteUserID(c)
	return c.NoContent(204)
}
func getEventsFunc(c echo.Context) error {
	events, err := getEvents(true)
	if err != nil {
		return err
	}
	for i, v := range events {
		events[i] = sanitizeEvent(v)
	}
	return c.JSON(200, events)
}
func getEventById(c echo.Context) error {
	eventID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return resError(c, "not_found", 404)
	}

	loginUserID := int64(-1)
	if user, err := getLoginUser(c); err == nil {
		loginUserID = user.ID
	}

	event, err := getEvent(eventID, loginUserID)
	if err != nil {
		if err == sql.ErrNoRows {
			return resError(c, "not_found", 404)
		}
		return err
	} else if !event.PublicFg {
		return resError(c, "not_found", 404)
	}
	return c.JSON(200, sanitizeEvent(event))
}
func postReservation(c echo.Context) error {
	eventID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return resError(c, "not_found", 404)
	}
	var params struct {
		Rank string `json:"sheet_rank"`
	}
	c.Bind(&params)

	user, err := getLoginUser(c)
	if err != nil {
		return err
	}

	event, err := getEvent(eventID, user.ID)
	if err != nil {
		if err == sql.ErrNoRows {
			return resError(c, "invalid_event", 404)
		}
		return err
	} else if !event.PublicFg {
		return resError(c, "invalid_event", 404)
	}

	if !validateRank(params.Rank) {
		return resError(c, "invalid_rank", 400)
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	var sheet Sheet
	var reservationID int64
	if err := tx.QueryRow("SELECT * FROM sheets WHERE id NOT IN (SELECT sheet_id FROM reservations WHERE event_id = ? AND canceled_at IS NULL FOR UPDATE) AND `rank` = ? ORDER BY RAND() LIMIT 1", event.ID, params.Rank).Scan(&sheet.ID, &sheet.Rank, &sheet.Num, &sheet.Price); err != nil {
		if err == sql.ErrNoRows {
			return resError(c, "sold_out", 409)
		}
		return err
	}
	res, err := tx.Exec("INSERT INTO reservations (event_id, sheet_id, user_id, reserved_at) VALUES (?, ?, ?, ?)", event.ID, sheet.ID, user.ID, time.Now().UTC().Format("2006-01-02 15:04:05.000000"))
	if err != nil {
		tx.Rollback()
		return err
	}
	reservationID, err = res.LastInsertId()
	if err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		tx.Rollback()
		return err
	}
	return c.JSON(202, echo.Map{
		"id":         reservationID,
		"sheet_rank": params.Rank,
		"sheet_num":  sheet.Num,
	})
}
func deleteReservation(c echo.Context) error {
	eventID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return resError(c, "not_found", 404)
	}
	rank := c.Param("rank")
	num := c.Param("num")

	user, err := getLoginUser(c)
	if err != nil {
		return err
	}

	event, err := getEvent(eventID, user.ID)
	if err != nil {
		if err == sql.ErrNoRows {
			return resError(c, "invalid_event", 404)
		}
		return err
	} else if !event.PublicFg {
		return resError(c, "invalid_event", 404)
	}

	if !validateRank(rank) {
		return resError(c, "invalid_rank", 404)
	}

	var sheet Sheet
	if err := db.QueryRow("SELECT * FROM sheets WHERE `rank` = ? AND num = ?", rank, num).Scan(&sheet.ID, &sheet.Rank, &sheet.Num, &sheet.Price); err != nil {
		if err == sql.ErrNoRows {
			return resError(c, "invalid_sheet", 404)
		}
		return err
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}

	var reservation Reservation
	if err := tx.QueryRow("SELECT * FROM reservations WHERE event_id = ? AND sheet_id = ? AND canceled_at IS NULL GROUP BY event_id HAVING reserved_at = MIN(reserved_at)", event.ID, sheet.ID).Scan(&reservation.ID, &reservation.EventID, &reservation.SheetID, &reservation.UserID, &reservation.ReservedAt, &reservation.CanceledAt); err != nil {
		tx.Rollback()
		if err == sql.ErrNoRows {
			return resError(c, "not_reserved", 400)
		}
		return err
	}
	if reservation.UserID != user.ID {
		tx.Rollback()
		return resError(c, "not_permitted", 403)
	}

	if _, err := tx.Exec("UPDATE reservations SET canceled_at = ? WHERE id = ?", time.Now().UTC().Format("2006-01-02 15:04:05.000000"), reservation.ID); err != nil {
		tx.Rollback()
		return err
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	return c.NoContent(204)
}
func getAdmin(c echo.Context) error {
	var events []*Event
	administrator := c.Get("administrator")
	if administrator != nil {
		var err error
		if events, err = getEvents(true); err != nil {
			return err
		}
	}
	return c.Render(200, "admin.tmpl", echo.Map{
		"events":        events,
		"administrator": administrator,
		"origin":        c.Scheme() + "://" + c.Request().Host,
	})
}
func postAdminLogin(c echo.Context) error {
	var params struct {
		LoginName string `json:"login_name"`
		Password  string `json:"password"`
	}
	c.Bind(&params)

	admin, ok := ln2admins[params.LoginName]
	if !ok {
		return resError(c, "authentication_failed", 401)
	}
	administrator := &admin

	var passHash string
	if err := db.QueryRow("SELECT SHA2(?, 256)", params.Password).Scan(&passHash); err != nil {
		return err
	}
	if administrator.PassHash != passHash {
		return resError(c, "authentication_failed", 401)
	}

	sessSetAdministratorID(c, administrator.ID)
	administrator, err := getLoginAdministrator(c)
	if err != nil {
		return err
	}
	return c.JSON(200, administrator)
}
func postAdminLogout(c echo.Context) error {
	sessDeleteAdministratorID(c)
	return c.NoContent(204)
}
func getAdminEvents(c echo.Context) error {
	events, err := getEvents(true)
	if err != nil {
		return err
	}
	return c.JSON(200, events)
}
func postAdminEvents(c echo.Context) error {
	var params struct {
		Title  string `json:"title"`
		Public bool   `json:"public"`
		Price  int    `json:"price"`
	}
	c.Bind(&params)

	tx, err := db.Begin()
	if err != nil {
		return err
	}

	res, err := tx.Exec("INSERT INTO events (title, public_fg, closed_fg, price) VALUES (?, ?, 0, ?)", params.Title, params.Public, params.Price)
	if err != nil {
		tx.Rollback()
		return err
	}
	eventID, err := res.LastInsertId()
	if err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	event, err := getEvent(eventID, -1)
	if err != nil {
		return err
	}
	return c.JSON(200, event)
}
func getAdminEventById(c echo.Context) error {
	eventID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return resError(c, "not_found", 404)
	}
	event, err := getEvent(eventID, -1)
	if err != nil {
		if err == sql.ErrNoRows {
			return resError(c, "not_found", 404)
		}
		return err
	}
	return c.JSON(200, event)
}

func postAdminEdit(c echo.Context) error {
	eventID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return resError(c, "not_found", 404)
	}

	var params struct {
		Public bool `json:"public"`
		Closed bool `json:"closed"`
	}
	c.Bind(&params)
	if params.Closed {
		params.Public = false
	}

	event, err := getEvent(eventID, -1)
	if err != nil {
		if err == sql.ErrNoRows {
			return resError(c, "not_found", 404)
		}
		return err
	}

	if event.ClosedFg {
		return resError(c, "cannot_edit_closed_event", 400)
	} else if event.PublicFg && params.Closed {
		return resError(c, "cannot_close_public_event", 400)
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec("UPDATE events SET public_fg = ?, closed_fg = ? WHERE id = ?", params.Public, params.Closed, event.ID); err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	e, err := getEvent(eventID, -1)
	if err != nil {
		return err
	}
	c.JSON(200, e)
	return nil
}

func getAdminEventSaleById(c echo.Context) error {
	eventID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return resError(c, "not_found", 404)
	}

	rows, err := db.Query("SELECT r.*, s.rank AS sheet_rank, s.num AS sheet_num, s.price AS sheet_price FROM reservations r INNER JOIN sheets s ON s.id = r.sheet_id WHERE r.event_id = ? ORDER BY r.id", eventID)
	if err != nil {
		return err
	}
	defer rows.Close()

	var reports []Report
	for rows.Next() {
		var reservation Reservation
		var sheet Sheet
		if err := rows.Scan(&reservation.ID, &reservation.EventID, &reservation.SheetID, &reservation.UserID, &reservation.ReservedAt, &reservation.CanceledAt, &sheet.Rank, &sheet.Num, &sheet.Price); err != nil {
			return err
		}
		report := Report{
			ReservationID: reservation.ID,
			EventID:       eventID,
			Rank:          sheet.Rank,
			Num:           sheet.Num,
			UserID:        reservation.UserID,
			SoldAt:        reservation.ReservedAt.Format("2006-01-02T15:04:05.000000Z"),
			Price:         eventPrice[eventID] + sheet.Price,
		}
		if reservation.CanceledAt != nil {
			report.CanceledAt = reservation.CanceledAt.Format("2006-01-02T15:04:05.000000Z")
		}
		reports = append(reports, report)
	}
	return renderReportCSV(c, reports)
}

var adminFewTimeMutex sync.Mutex

func getAdminEventsSales(c echo.Context) error {
	tick := time.After(50 * time.Second)
	adminFewTimeMutex.Lock()
	defer func() {
		adminFewTimeMutex.Unlock()
	}()
	//TODO: ここを直す
	rows, err := db.Query("select r.*, s.rank as sheet_rank, s.num as sheet_num, s.price as sheet_price from reservations r inner join sheets s on s.id = r.sheet_id order by r.id")
	if err != nil {
		return err
	}
	defer rows.Close()

	var reports []Report
	for rows.Next() {
		var reservation Reservation
		var sheet Sheet
		if err := rows.Scan(&reservation.ID, &reservation.EventID, &reservation.SheetID, &reservation.UserID, &reservation.ReservedAt, &reservation.CanceledAt, &sheet.Rank, &sheet.Num, &sheet.Price); err != nil {
			return err
		}
		report := Report{
			ReservationID: reservation.ID,
			//todo
			EventID:/*event.ID*/ reservation.EventID,
			Rank:   sheet.Rank,
			Num:    sheet.Num,
			UserID: reservation.UserID,
			SoldAt: reservation.ReservedAt.Format("2006-01-02T15:04:05.000000Z"),
			// TODO koko
			Price:/*Event.Price*/ eventPrice[reservation.EventID] + sheet.Price,
		}
		if reservation.CanceledAt != nil {
			report.CanceledAt = reservation.CanceledAt.Format("2006-01-02T15:04:05.000000Z")
		}
		reports = append(reports, report)
	}
	err = renderReportCSV(c, reports)
	<-tick
	return err
}

func main() {
	dsn := fmt.Sprintf("%s:%s@tcp(%s:%s)/%s?parseTime=true&charset=utf8mb4",
		os.Getenv("DB_USER"), os.Getenv("DB_PASS"),
		os.Getenv("DB_HOST"), os.Getenv("DB_PORT"),
		os.Getenv("DB_DATABASE"),
	)
	var err error
	db, err = sql.Open("mysql", dsn)
	if err != nil {
		log.Fatal(err)
	}
	e := echo.New()
	// Group, Middleware and Routes for /debug/* from Go's stdlib
	// GET handlers (or POST if it needs)
	echopprof.Wrap(e)
	funcs := template.FuncMap{
		"encode_json": encodeJson,
	}
	e.Renderer = &Renderer{
		templates: template.Must(template.New("").Delims("[[", "]]").Funcs(funcs).ParseGlob("views/*.tmpl")),
	}
	e.Use(session.Middleware(sessions.NewCookieStore([]byte("secret"))))
	e.Use(middleware.LoggerWithConfig(middleware.LoggerConfig{Output: os.Stderr}))
	e.Static("/", "public")
	e.GET("/", getIndex, fillinUser)
	e.GET("/initialize", getInitialize)
	e.POST("/api/users", postUsers)
	e.GET("/api/users/:id", getUser, loginRequired)
	e.POST("/api/actions/login", postLogin)
	e.POST("/api/actions/logout", postLogout, loginRequired)
	e.GET("/api/events", getEventsFunc)
	e.GET("/api/events/:id", getEventById)
	e.POST("/api/events/:id/actions/reserve", postReservation, loginRequired)
	e.DELETE("/api/events/:id/sheets/:rank/:num/reservation", deleteReservation, loginRequired)
	e.GET("/admin/", getAdmin, fillinAdministrator)
	e.POST("/admin/api/actions/login", postAdminLogin)
	e.POST("/admin/api/actions/logout", postAdminLogout, adminLoginRequired)
	e.GET("/admin/api/events", getAdminEvents, adminLoginRequired)
	e.POST("/admin/api/events", postAdminEvents, adminLoginRequired)
	e.GET("/admin/api/events/:id", getAdminEventById, adminLoginRequired)
	e.POST("/admin/api/events/:id/actions/edit", postAdminEdit, adminLoginRequired)
	e.GET("/admin/api/reports/events/:id/sales", getAdminEventSaleById, adminLoginRequired)
	e.GET("/admin/api/reports/sales", getAdminEventsSales, adminLoginRequired)
	e.Start(":8080")
}

type Report struct {
	ReservationID int64
	EventID       int64
	Rank          string
	Num           int64
	UserID        int64
	SoldAt        string
	CanceledAt    string
	Price         int64
}

func renderReportCSV(c echo.Context, reports []Report) error {
	body := bytes.NewBufferString("reservation_id,event_id,rank,num,price,user_id,sold_at,canceled_at\n")
	for _, v := range reports {
		body.WriteString(fmt.Sprintf("%d,%d,%s,%d,%d,%d,%s,%s\n",
			v.ReservationID, v.EventID, v.Rank, v.Num, v.Price, v.UserID, v.SoldAt, v.CanceledAt))
	}

	c.Response().Header().Set("Content-Type", `text/csv; charset=UTF-8`)
	c.Response().Header().Set("Content-Disposition", `attachment; filename="report.csv"`)
	_, err := io.Copy(c.Response(), body)
	return err
}

func resError(c echo.Context, e string, status int) error {
	if e == "" {
		e = "unknown"
	}
	if status < 100 {
		status = 500
	}
	return c.JSON(status, map[string]string{"error": e})
}
