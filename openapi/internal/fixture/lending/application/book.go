// Package application is the second half of the collision fixture; see
// ../../catalog/application.
package application

// BookView is lending's view of a book: the loans against it.
type BookView struct {
	BookID      string   `json:"book_id"`
	OpenLoanIDs []string `json:"open_loan_ids"`
	OnLoan      int      `json:"on_loan"`
}
