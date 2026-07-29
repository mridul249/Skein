//go:build ignore

// Scratch measurement used to disprove the ordering hypothesis for issue #7.
// Excluded from the build; delete this file. Kept only because the shell tool
// was unavailable to remove it. Its results are recorded in the commit message
// and in the admitPlanning doc comment:
//
//	1 account  10GiB  2x8GiB  most-available: both=0 one=189 NONE=11
//	1 account  10GiB  2x8GiB  priority      : both=0 one=178 NONE=22
//	2 accounts 5GiB ea 2x8GiB priority      : both=0 one=173 NONE=27
//	3 accounts 4GiB ea 2x8GiB priority      : both=0 one=171 NONE=29
package router
