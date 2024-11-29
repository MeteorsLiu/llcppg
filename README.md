llcppg - LLGo autogen tool for C/C++ libraries
====

## How to install

This project depends on LLGO's C ecosystem integration capability, and some components of this tool must be compiled with LLGO. For LLGO installation, please refer to:
https://github.com/goplus/llgo?tab=readme-ov-file#how-to-install

```bash
brew install cjson # macos
apt-get install libcjson-dev # linux
llgo install ./_xtool/llcppsymg
llgo install ./_xtool/llcppsigfetch
go install ./cmd/gogensig
go install .
```

## Usage

```sh
llcppg [config-file]
```

If `config-file` is not specified, a `llcppg.cfg` file is used in current directory. 
Here's a demo configuration to generate LLGO bindings for cjson library:

```json
{
  "name": "cjson",
  "cflags": "$(pkg-config --cflags libcjson)",
  "include": ["cJSON.h","cJSON_Utils.h"],
  "libs": "$(pkg-config --libs libcjson libcjson_utils)",
  "trimPrefixes": ["cJSONUtils_","cJSON_"]
}
```

After creating the configuration file, run:

```bash
llcppg llcppg.cfg
```

After execution, a Go project will be generated in a directory named after the config name (which is also the package name). For example, with the cjson configuration above, you'll see:

```bash
cjson/
├── cJSON.go
├── cJSON_Utils.go
├── cjson_autogen_link.go
├── go.mod
└── go.sum
```

Import the generated cjson package and try this demo:

```go
package main

import (
    "cjson"
    "github.com/goplus/llgo/c"
)

func main() {
    mod := cjson.CreateObject()
    cjson.AddItemToObject(mod, c.Str("hello"), cjson.CreateString(c.Str("llgo")))
    cjson.AddItemToObject(mod, c.Str("hello"), cjson.CreateString(c.Str("llcppg")))
    cstr := cjson.PrintUnformatted(mod)
    c.Printf(c.Str("%s\n"), cstr)
}
```
Run the demo with `llgo run .`, you will see the following output:
```
{"hello":"llgo","hello":"llcppg"}
```

### Customize generated name

When you run llcppg directly with the above configuration, it will generate function names according to the configuration. After execution, you'll find a `llcppg.symb.json` file in the current directory. 

```json
[
  {
    "mangle": "cJSON_CreateArray",
    "c++": "cJSON_CreateArray()",
    "go": "CreateArray"
  },
  {
    "mangle": "cJSON_CreateArrayReference",
    "c++": "cJSON_CreateArrayReference(const cJSON *)",
    "go": "CreateArrayReference"
  },
  {
    "mangle": "cJSON_CreateBool",
    "c++": "cJSON_CreateBool(cJSON_bool)",
    "go": "CreateBool"
  },
  {
    "mangle": "cJSON_CreateDoubleArray",
    "c++": "cJSON_CreateDoubleArray(const double *, int)",
    "go": "CreateDoubleArray"
  }
]
```

- `mangle` field contains the symbol name of function
- `c++` field shows the function prototype from the header file
- `go` field displays the function name that will be generated. You can customize the generated function names by modifying this field in the `llcppg.symb.json` file. For example, you can simplify the names by changing "CreateArray" to "Array", "CreateBool" to "Bool", etc.

After modifying the file, run llcppg again to apply your customized function names.

The symbol table is generated by llcppsymg, which is internally called by llcppg to generate the symbol table as input for Go code generation. You can also run llcppsymg separately to customize the symbol table before running llcppg.

## Design

See [llcppg Design](design.md).
