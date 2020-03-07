// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/go-interpreter/wagon/wasm"
	"golang.org/x/tools/go/packages"
)

func main() {
	if err := run(); err != nil {
		panic(err)
	}
}

func identifierFromString(str string) string {
	var ident string
	for _, r := range []rune(str) {
		if r > 0xff {
			panic("identifiers cannot include non-Latin1 characters")
		}
		if '0' <= r && r <= '9' {
			ident += string(r)
			continue
		}
		if 'a' <= r && r <= 'z' {
			ident += string(r)
			continue
		}
		if 'A' <= r && r <= 'Z' {
			ident += string(r)
			continue
		}
		ident += fmt.Sprintf("_%02x", r)
	}
	return ident
}

func namespaceFromPkg(pkg *packages.Package) string {
	ts := strings.Split(pkg.PkgPath, "/")
	for i, t := range ts {
		ts[i] = identifierFromString(t)
	}
	ts = append([]string{"Go2DotNet", "AutoGen"}, ts...)
	return strings.Join(ts, ".")
}

type Func struct {
	Mod     *wasm.Module
	Funcs   []*Func
	Types   []*Type
	Type    *Type
	Wasm    wasm.Function
	Index   int
	Import  bool
	BodyStr string
}

func (f *Func) Identifier() string {
	return identifierFromString(f.Wasm.Name)
}

var funcTmpl = template.Must(template.New("func").Parse(`// OriginalName: {{.OriginalName}}
// Index:        {{.Index}}
{{if .WithBody}}{{if .Public}}public{{else}}private{{end}} {{end}}{{.ReturnType}} {{.Name}}({{.Args}}){{if .WithBody}}
{
{{range .Locals}}    {{.}}
{{end}}{{if .Locals}}
{{end}}{{range .Body}}{{.}}
{{end}}}{{else}};{{end}}`))

func wasmTypeToReturnType(v wasm.ValueType) ReturnType {
	switch v {
	case wasm.ValueTypeI32:
		return ReturnTypeI32
	case wasm.ValueTypeI64:
		return ReturnTypeI64
	case wasm.ValueTypeF32:
		return ReturnTypeF32
	case wasm.ValueTypeF64:
		return ReturnTypeF64
	default:
		panic("not reached")
	}
}

func (f *Func) CSharp(indent string, public bool, withBody bool) (string, error) {
	var retType ReturnType
	switch ts := f.Wasm.Sig.ReturnTypes; len(ts) {
	case 0:
		retType = ReturnTypeVoid
	case 1:
		retType = wasmTypeToReturnType(ts[0])
	default:
		return "", fmt.Errorf("the number of return values must be 0 or 1 but %d", len(ts))
	}

	var args []string
	for i, t := range f.Wasm.Sig.ParamTypes {
		args = append(args, fmt.Sprintf("%s local%d", wasmTypeToReturnType(t).CSharp(), i))
	}

	var locals []string
	var body []string
	if withBody {
		if f.BodyStr != "" {
			body = strings.Split(f.BodyStr, "\n")
		} else if f.Wasm.Body != nil {
			var idx int
			for _, e := range f.Wasm.Body.Locals {
				for i := 0; i < int(e.Count); i++ {
					locals = append(locals, fmt.Sprintf("%s local%d = 0;", wasmTypeToReturnType(e.Type).CSharp(), idx+len(f.Wasm.Sig.ParamTypes)))
					idx++
				}
			}
			var err error
			body, err = f.bodyToCSharp()
			if err != nil {
				return "", err
			}
		} else {
			body = []string{"    throw new NotImplementedException();"}
		}
	}

	var buf bytes.Buffer
	if err := funcTmpl.Execute(&buf, struct {
		OriginalName string
		Name         string
		Index        int
		ReturnType   string
		Args         string
		Locals       []string
		Body         []string
		Public       bool
		WithBody     bool
	}{
		OriginalName: f.Wasm.Name,
		Name:         identifierFromString(f.Wasm.Name),
		Index:        f.Index,
		ReturnType:   retType.CSharp(),
		Args:         strings.Join(args, ", "),
		Locals:       locals,
		Body:         body,
		Public:       public,
		WithBody:     withBody,
	}); err != nil {
		return "", err
	}

	// Add indentations
	var lines []string
	for _, line := range strings.Split(buf.String(), "\n") {
		lines = append(lines, indent+line)
	}
	return strings.Join(lines, "\n") + "\n", nil
}

type Export struct {
	Funcs []*Func
	Index int
	Name  string
}

func (e *Export) CSharp(indent string) (string, error) {
	f := e.Funcs[e.Index]

	var ret string
	var retType ReturnType
	switch ts := f.Wasm.Sig.ReturnTypes; len(ts) {
	case 0:
		retType = ReturnTypeVoid
	case 1:
		ret = "return "
		retType = wasmTypeToReturnType(ts[0])
	default:
		return "", fmt.Errorf("the number of return values must be 0 or 1 but %d", len(ts))
	}

	var args []string
	var argsToPass []string
	for i, t := range f.Wasm.Sig.ParamTypes {
		args = append(args, fmt.Sprintf("%s arg%d", wasmTypeToReturnType(t).CSharp(), i))
		argsToPass = append(argsToPass, fmt.Sprintf("arg%d", i))
	}

	str := fmt.Sprintf(`public %s %s(%s)
{
    %s%s(%s);
}
`, retType.CSharp(), e.Name, strings.Join(args, ", "), ret, identifierFromString(f.Wasm.Name), strings.Join(argsToPass, ", "))

	lines := strings.Split(str, "\n")
	for i := range lines {
		lines[i] = indent + lines[i]
	}
	return strings.Join(lines, "\n"), nil
}

type Global struct {
	Type  wasm.ValueType
	Index int
	Init  int
}

func (g *Global) CSharp(indent string) string {
	return fmt.Sprintf("%sprivate %s global%d = %d;", indent, wasmTypeToReturnType(g.Type).CSharp(), g.Index, g.Init)
}

type Type struct {
	Sig   *wasm.FunctionSig
	Index int
}

func (t *Type) CSharp(indent string) (string, error) {
	var retType ReturnType
	switch ts := t.Sig.ReturnTypes; len(ts) {
	case 0:
		retType = ReturnTypeVoid
	case 1:
		retType = wasmTypeToReturnType(ts[0])
	default:
		return "", fmt.Errorf("the number of return values must be 0 or 1 but %d", len(ts))
	}

	var args []string
	for i, t := range t.Sig.ParamTypes {
		args = append(args, fmt.Sprintf("%s arg%d", wasmTypeToReturnType(t).CSharp(), i))
	}

	return fmt.Sprintf("%sprivate delegate %s Type%d(%s);", indent, retType.CSharp(), t.Index, strings.Join(args, ", ")), nil
}

func run() error {
	tmp, err := ioutil.TempDir("", "go2dotnet-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	wasmpath := filepath.Join(tmp, "tmp.wasm")

	// TODO: Detect the last argument is path or not
	pkgname := os.Args[len(os.Args)-1]

	args := append([]string{"build"}, os.Args[1:]...)
	args = append(args[:len(args)-1], "-o="+wasmpath, pkgname)
	cmd := exec.Command("go", args...)
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(), "GOOS=js", "GOARCH=wasm")
	if err := cmd.Run(); err != nil {
		return err
	}

	f, err := os.Open(wasmpath)
	if err != nil {
		return err
	}
	defer f.Close()

	mod, err := wasm.ReadModule(f, nil)
	if err != nil {
		return err
	}

	var types []*Type
	for i, e := range mod.Types.Entries {
		e := e
		types = append(types, &Type{
			Sig:   &e,
			Index: i,
		})
	}

	var ifs []*Func
	var fs []*Func
	for i, f := range mod.FunctionIndexSpace {
		// There is a bug that signature and body are shifted (go-interpreter/wagon#190).
		// TODO: Avoid using FunctionIndexSpace?
		if f.Name == "" {
			name := mod.Import.Entries[i].FieldName
			ifs = append(ifs, &Func{
				Type: types[mod.Import.Entries[i].Type.(wasm.FuncImport).Type],
				Wasm: wasm.Function{
					Sig:  types[mod.Import.Entries[i].Type.(wasm.FuncImport).Type].Sig,
					Name: name,
				},
				Index:   i,
				Import:  true,
				BodyStr: importFuncBodies[name],
			})
			continue
		}

		f2 := mod.FunctionIndexSpace[i-len(mod.Import.Entries)]
		fs = append(fs, &Func{
			Type: types[mod.Function.Types[i-len(mod.Import.Entries)]],
			Wasm: wasm.Function{
				Sig:  types[mod.Function.Types[i-len(mod.Import.Entries)]].Sig,
				Body: f2.Body,
				Name: f.Name,
			},
			Index: i,
		})
	}

	var exports []*Export
	for _, e := range mod.Export.Entries {
		switch e.Kind {
		case wasm.ExternalFunction:
			exports = append(exports, &Export{
				Index: int(e.Index),
				Name:  e.FieldStr,
			})
		case wasm.ExternalMemory:
			// Ignore
		default:
			return fmt.Errorf("export type %d is not implemented", e.Kind)
		}
	}

	allfs := append(ifs, fs...)
	for _, e := range exports {
		e.Funcs = allfs
	}
	for _, f := range ifs {
		f.Mod = mod
		f.Funcs = allfs
		f.Types = types
	}
	for _, f := range fs {
		f.Mod = mod
		f.Funcs = allfs
		f.Types = types
	}

	var globals []*Global
	for i, e := range mod.Global.Globals {
		// TODO: Consider mutability.
		// TODO: Use e.Type.Init.
		globals = append(globals, &Global{
			Type:  e.Type.Type,
			Index: i,
			Init:  0,
		})
	}

	if mod.Start != nil {
		return fmt.Errorf("start section must be nil but not")
	}

	pkgs, err := packages.Load(nil, pkgname)
	if err != nil {
		return err
	}

	namespace := namespaceFromPkg(pkgs[0])
	class := identifierFromString(pkgs[0].Name)

	if err := csTmpl.Execute(os.Stdout, struct {
		Namespace   string
		Class       string
		ImportFuncs []*Func
		Funcs       []*Func
		Exports     []*Export
		Globals     []*Global
		Types       []*Type
		Table       [][]uint32
		InitPageNum int
		InitMem     []byte
	}{
		Namespace:   namespace,
		Class:       class,
		ImportFuncs: ifs,
		Funcs:       fs,
		Exports:     exports,
		Globals:     globals,
		Types:       types,
		Table:       mod.TableIndexSpace,
		InitPageNum: int(mod.Memory.Entries[0].Limits.Initial),
		InitMem:     mod.LinearMemoryIndexSpace[0],
	}); err != nil {
		return err
	}

	return nil
}

var csTmpl = template.Must(template.New("out.cs").Parse(`// Code generated by go2dotnet. DO NOT EDIT.

#pragma warning disable 162 // unreachable code
#pragma warning disable 164 // label
#pragma warning disable 219 // unused local variables

using System;
using System.Collections.Generic;
using System.Diagnostics;
using System.Linq;
using System.Runtime.CompilerServices;
using System.Security.Cryptography;
using System.Text;
using System.Threading.Tasks;
using System.Timers;

namespace {{.Namespace}}
{
    sealed class Mem
    {
        const int PageSize = 64 * 1024;

        public Mem()
        {
            this.bytes = new byte[{{.InitPageNum}} * PageSize];
            byte[] src = new byte[] { {{- range $value := .InitMem}}{{$value}},{{end -}} };
            Array.Copy(src, this.bytes, src.Length);
        }

        internal int Size
        {
            get
            {
                return this.bytes.Length;
            }
        }

        internal int Grow(int delta)
        {
            var prevSize = this.Size;
            var newSize = prevSize + delta;
            if (newSize % PageSize != 0)
            {
                newSize += PageSize - newSize % PageSize;
            }
            Array.Resize(ref this.bytes, newSize);
            return prevSize;
        }

        internal sbyte LoadInt8(int addr)
        {
            return (sbyte)this.bytes[addr];
        }

        internal byte LoadUint8(int addr)
        {
            return this.bytes[addr];
        }

        internal short LoadInt16(int addr)
        {
            return (short)((ushort)this.bytes[addr] | (ushort)(this.bytes[addr+1]) << 8);
        }

        internal ushort LoadUint16(int addr)
        {
            return (ushort)((ushort)this.bytes[addr] | (ushort)(this.bytes[addr+1]) << 8);
        }

        internal int LoadInt32(int addr)
        {
            return (int)((uint)this.bytes[addr] |
                (uint)(this.bytes[addr+1]) << 8 |
                (uint)this.bytes[addr+2] << 16 |
                (uint)(this.bytes[addr+3]) << 24);
        }

        internal uint LoadUint32(int addr)
        {
            return (uint)((uint)this.bytes[addr] |
                (uint)(this.bytes[addr+1]) << 8 |
                (uint)this.bytes[addr+2] << 16 |
                (uint)(this.bytes[addr+3]) << 24);
        }

        internal long LoadInt64(int addr)
        {
            return (long)((ulong)this.bytes[addr] |
                (ulong)(this.bytes[addr+1]) << 8 |
                (ulong)this.bytes[addr+2] << 16 |
                (ulong)(this.bytes[addr+3]) << 24 |
                (ulong)(this.bytes[addr+4]) << 32 |
                (ulong)(this.bytes[addr+5]) << 40 |
                (ulong)(this.bytes[addr+6]) << 48 |
                (ulong)(this.bytes[addr+7]) << 54);
        }

        internal float LoadFloat32(int addr)
        {
            int bits = LoadInt32(addr);
            return Unsafe.As<int, float>(ref bits);
        }

        internal double LoadFloat64(int addr)
        {
            long bits = LoadInt64(addr);
            return Unsafe.As<long, double>(ref bits);
        }

        internal void StoreInt8(int addr, sbyte val)
        {
            this.bytes[addr] = (byte)val;
        }

        internal void StoreInt16(int addr, short val)
        {
            this.bytes[addr] = (byte)val;
            this.bytes[addr+1] = (byte)(val >> 8);
        }

        internal void StoreInt32(int addr, int val)
        {
            this.bytes[addr] = (byte)val;
            this.bytes[addr+1] = (byte)(val >> 8);
            this.bytes[addr+2] = (byte)(val >> 16);
            this.bytes[addr+3] = (byte)(val >> 24);
        }

        internal void StoreInt64(int addr, long val)
        {
            this.bytes[addr] = (byte)val;
            this.bytes[addr+1] = (byte)(val >> 8);
            this.bytes[addr+2] = (byte)(val >> 16);
            this.bytes[addr+3] = (byte)(val >> 24);
            this.bytes[addr+4] = (byte)(val >> 32);
            this.bytes[addr+5] = (byte)(val >> 40);
            this.bytes[addr+6] = (byte)(val >> 48);
            this.bytes[addr+7] = (byte)(val >> 54);
        }

        internal void StoreFloat32(int addr, float val)
        {
            this.StoreInt32(addr, Unsafe.As<float, int>(ref val));
        }

        internal void StoreFloat64(int addr, double val)
        {
            this.StoreInt64(addr, Unsafe.As<double, long>(ref val));
        }

        internal void StoreBytes(int addr, byte[] bytes)
        {
            for (int i = 0; i < bytes.Length; i++)
            {
                this.bytes[addr+i] = bytes[i];
            }
        }

        internal ArraySegment<byte> LoadSlice(int addr)
        {
            var array = this.LoadInt64(addr);
            var len = this.LoadInt64(addr + 8);
            return new ArraySegment<byte>(this.bytes, (int)array, (int)len);
        }

        internal ArraySegment<byte> LoadSliceDirectly(long array, int len)
        {
            return new ArraySegment<byte>(this.bytes, (int)array, len);
        }

        private byte[] bytes;
    }

    internal interface IImport
    {
{{- range $value := .ImportFuncs}}
{{$value.CSharp "        " false false}}{{end}}
    }

    public class Go
    {
        class Import : IImport
        {
            internal Import(Go go)
            {
                this.go = go;
            }
{{range $value := .ImportFuncs}}
{{$value.CSharp "            " true true}}{{end}}
            private Go go;
        }

        public Go()
        {
            this.import = new Import(this);
            this.exitPromise = new TaskCompletionSource<int>();
        }

        public Task Run(string[] args)
        {
            this.buf = new List<byte>();
            this.stopwatch = Stopwatch.StartNew();
            this.mem = new Mem();
            this.inst = new Go_{{.Class}}(this.mem, this.import);
            this.values = new Dictionary<int, object>
            {
                {0, double.NaN},
                {1, 0},
                {2, null},
                {3, true},
                {4, false},
                {5, null}, // TODO: Add a pseudo 'global' object.
                {6, this},
            };
            this.goRefCounts = new Dictionary<int, int>();
            this.ids = new Dictionary<int, int>();
            this.idPool = new HashSet<int>();
            this.exited = false;

            int offset = 4096;
            Func<string, int> strPtr = (string str) => {
                int ptr = offset;
                byte[] bytes = Encoding.UTF8.GetBytes(str + '\0');
                this.mem.StoreBytes(offset, bytes);
                offset += bytes.Length;
                if (offset % 8 != 0)
                {
                    offset += 8 - (offset % 8);
                }
                return ptr;
            };

            // 'js' is requried as the first argument.
            int argc = args.Length + 1;
            IEnumerable<int> argvPtrs = args.Prepend("js").Select(arg => strPtr(arg)).Append(0);
            // TODO: Add environment variables.
            argvPtrs = argvPtrs.Append(0);

            int argv = offset;
            foreach (int ptr in argvPtrs)
            {
                this.mem.StoreInt32(offset, ptr);
                this.mem.StoreInt32(offset + 4, 0);
                offset += 8;
            }

            this.inst.run(argc, argv);
            if (this.exited)
            {
                this.exitPromise.SetResult(0);
            }
            return this.exitPromise.Task;
        }

        private void Exit(int code)
        {
            if (code != 0)
            {
                Console.Error.WriteLine($"exit code: {code}");
            }
        }

        private void Resume()
        {
            if (this.exited)
            {
                throw new Exception("Go program has already exited");
            }
            this.inst.resume();
            if (this.exited)
            {
                this.exitPromise.SetResult(0);
            }
        }

        private void DebugWrite(IEnumerable<byte> bytes)
        {
            this.buf.AddRange(bytes);
            while (this.buf.Contains((byte)'\n'))
            {
                var idx = this.buf.IndexOf((byte)'\n');
                var str = Encoding.UTF8.GetString(this.buf.GetRange(0, idx).ToArray());
                Console.WriteLine(str);
                this.buf.RemoveRange(0, idx+1);
            }
        }

        private long PreciseNowInNanoseconds()
        {
            return this.stopwatch.ElapsedTicks * nanosecPerTick;
        }

        private double UnixNowInMilliseconds()
        {
            return (DateTime.UtcNow.Subtract(new DateTime(1970, 1, 1))).TotalMilliseconds;
        }

        private int SetTimeout(double interval)
        {
            var id = this.nextCallbackTimeoutId;
            this.nextCallbackTimeoutId++;

            Timer timer = new Timer(interval);
            timer.Elapsed += (sender, e) => {
                this.Resume();
                while (this.scheduledTimeouts.ContainsKey(id))
                {
                    // for some reason Go failed to register the timeout event, log and try again
                    // (temporary workaround for https://github.com/golang/go/issues/28975)
                    this.Resume();
                }
            };
            timer.AutoReset = false;
            timer.Start();

            this.scheduledTimeouts[id] = timer;

            return id;
        }

        private void ClearTimeout(int id)
        {
            if (this.scheduledTimeouts.ContainsKey(id))
            {
                this.scheduledTimeouts[id].Stop();
            }
            this.scheduledTimeouts.Remove(id);
        }

        private byte[] GetRandomBytes(int length)
        {
            var bytes = new byte[length];
            this.rngCsp.GetBytes(bytes);
            return bytes;
        }

        private static long nanosecPerTick = (1_000_000_000L) / Stopwatch.Frequency;

        private Import import;
        private TaskCompletionSource<int> exitPromise;

        private List<byte> buf;
        private Stopwatch stopwatch;

        private Dictionary<int, Timer> scheduledTimeouts = new Dictionary<int, Timer>();
        private int nextCallbackTimeoutId = 1;
        private Go_{{.Class}} inst;
        private Mem mem;
        private Dictionary<int, object> values;
        private Dictionary<int, int> goRefCounts;
        private Dictionary<int, int> ids;
        private HashSet<int> idPool;
        private bool exited;
        private RNGCryptoServiceProvider rngCsp = new RNGCryptoServiceProvider();
    }

    sealed class Go_{{.Class}}
    {
        public Go_{{.Class}}(Mem mem, IImport import)
        {
             initializeFuncs_();
             mem_ = mem;
             import_ = import;
        }

{{range $value := .Exports}}{{$value.CSharp "        "}}
{{end}}
{{range $value := .Funcs}}{{$value.CSharp "        " false true}}
{{end}}
{{range $value := .Types}}{{$value.CSharp "        "}}
{{end}}        private static readonly uint[][] table_ = {
{{range $value := .Table}}            new uint[] { {{- range $value2 := $value}}{{$value2}}, {{end}}},
{{end}}        };

        private void initializeFuncs_()
        {
            funcs_ = new object[] {
{{range $value := .ImportFuncs}}                null,
{{end}}{{range $value := .Funcs}}                (Type{{.Type.Index}})({{.Identifier}}),
{{end}}            };
        }

{{range $value := .Globals}}{{$value.CSharp "        "}}
{{end}}
        private object[] funcs_;
        private Mem mem_;
        private IImport import_;
    }
}
`))
