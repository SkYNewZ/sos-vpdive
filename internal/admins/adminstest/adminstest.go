// Package adminstest holds test helpers for packages that need a committee
// account: it must only be imported from tests.
package adminstest

// Password is the password of Hash.
const Password = "correct horse battery staple"

// Hash is Password hashed with argon2id at the lowest cost (m=8 KiB, t=1,
// p=1), which VerifyPassword reads from the string: a login against it takes
// microseconds, against HashPassword's 64 MiB about a second under -race.
const Hash = "$argon2id$v=19$m=8,t=1,p=1$dGVzdHNhbHR0ZXN0c2FsdA$frxalUL5r2jFZohAL3Xgn52Wgd4QUugr/XMUOJgwuTY"
