module github.com/SoftinoProducts/notifier-go-sdk

// Minimum toolchain. The standard library ships with the toolchain, so this line decides
// which stdlib security fixes a consumer's build contains. 1.26.3 fixed 11 stdlib CVEs
// (net/mail quadratic parsing, html/template escaper bypass, net/http/2 DoS, and others);
// 1.26.8 is the current patch release of that line.
go 1.26.8
