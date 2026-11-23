// Package errorclass re-exports the top-level errorclass package under its
// previous import path.
//
// Deprecated: import github.com/chimerakang/alice-core/errorclass instead.
package errorclass

import root "github.com/chimerakang/alice-core/errorclass"

// Class is errorclass.Class.
type Class = root.Class

const (
	ClassUnknown        = root.ClassUnknown
	ClassRateLimit      = root.ClassRateLimit
	ClassServerOverload = root.ClassServerOverload
	ClassNetworkReset   = root.ClassNetworkReset
	ClassTimeout        = root.ClassTimeout
	ClassEOF            = root.ClassEOF
	ClassContextTooLong = root.ClassContextTooLong
	ClassKilled         = root.ClassKilled
)

// Classify is errorclass.Classify.
func Classify(err error) Class { return root.Classify(err) }

// ClassifyText is errorclass.ClassifyText.
func ClassifyText(text string) Class { return root.ClassifyText(text) }
