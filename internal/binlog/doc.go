// Package binlog reads MySQL/MariaDB ROW-format binary log events — from a file
// or a live replica stream — and normalizes them into change events that the
// reverse engine can invert.
//
// Reversing UPDATE and DELETE requires the full prior row image, so the source
// database must run with binlog_format=ROW and binlog_row_image=FULL. Callers
// should verify this on connect and refuse otherwise.
package binlog
