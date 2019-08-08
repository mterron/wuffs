// Code generated by running "go generate". DO NOT EDIT.

// Copyright 2019 The Wuffs Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

const usageStr = `Usage:

ractool [flags] [input_filename]

If no input_filename is given, stdin is used. Either way, output is written to
stdout.

The flags should include exactly one of -decode or -encode.

When encoding, the input is partitioned into chunks and each chunk is
compressed independently. You can specify the target chunk size in terms of
either its compressed size or decompressed size. By default (if both
-cchunksize and -dchunksize are zero), a 64KiB -dchunksize is used.

You can also specify a -cpagesize, which is similar to but not exactly the same
concept as alignment. If non-zero, padding is inserted into the output to
minimize the number of pages that each chunk occupies. Look for "CPageSize" in
the "package rac" documentation for more details:
https://godoc.org/github.com/google/wuffs/lib/rac

A RAC file consists of an index and the chunks. The index may be either at the
start or at the end of the file. At the start results in slightly smaller and
slightly more efficient RAC files, but the encoding process needs more memory
or temporary disk space. See the RAC specification for more details:
https://github.com/google/wuffs/blob/master/doc/spec/rac-spec.md

Examples:

  ractool -decode foo.rac | sha256sum
  ractool -decode -drange=400:500 foo.rac
  ractool -encode foo.dat > foo.rac
  ractool -encode -codec=zlib -dchunksize=256 foo.dat > foo.raczlib

General Flags:

-decode
    whether to decode the input
-encode
    whether to encode the input

Decode-Related Flags:

-drange
    the "i:j" range to decompress, ":8" means the first 8 bytes

Encode-Related Flags:

-cchunksize
    the chunk size (in CSpace)
-codec
    the compression codec (default "zlib")
-cpagesize
    the page size (in CSpace)
-dchunksize
    the chunk size (in DSpace)
-indexlocation
    the index location, "start" or "end" (default "start")
`
