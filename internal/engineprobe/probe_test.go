package engineprobe

import (
 "context"
 "os"
 "path/filepath"
 "runtime"
 "strings"
 "testing"
 "time"
)
func TestForegroundProcess(t *testing.T){
 if runtime.GOOS!="linux"{t.Skip("Linux test")}
 path:="/bin/echo"
 if _,err:=os.Stat(path);err!=nil{t.Skip("echo unavailable")}
 r,err:=Run(context.Background(),Spec{Name:"echo",Executable:path,Args:[]string{"hello"},Timeout:time.Second,OutputLimit:128})
 if err!=nil||r.ExitCode!=0||strings.TrimSpace(r.Output)!="hello"{t.Fatalf("%+v %v",r,err)}
}
func TestNoShellInterpretation(t *testing.T){
 if _,err:=os.Stat("/bin/echo");err!=nil{t.Skip("echo unavailable")}
 r,err:=Run(context.Background(),Spec{Name:"echo",Executable:"/bin/echo",Args:[]string{"$(touch /tmp/routerd2-must-not-exist)"},Timeout:time.Second,OutputLimit:128})
 if err!=nil||!strings.Contains(r.Output,"$(touch"){t.Fatalf("%+v %v",r,err)}
}
func TestRejectSymlinkAndBadLimits(t *testing.T){
 dir:=t.TempDir(); link:=filepath.Join(dir,"echo")
 if err:=os.Symlink("/bin/echo",link);err!=nil{t.Fatal(err)}
 for _,s:=range []Spec{
 {Name:"echo",Executable:link,Timeout:time.Second,OutputLimit:10},
 {Name:"echo",Executable:"/bin/echo",Timeout:0,OutputLimit:10},
 {Name:"echo",Executable:"/bin/echo",Timeout:time.Second,OutputLimit:0},
 } {if _,err:=Run(context.Background(),s);err==nil{t.Fatalf("accepted %+v",s)}}
}
func TestTimeout(t *testing.T){
 if _,err:=os.Stat("/bin/sleep");err!=nil{t.Skip("sleep unavailable")}
 r,err:=Run(context.Background(),Spec{Name:"sleep",Executable:"/bin/sleep",Args:[]string{"2"},Timeout:30*time.Millisecond,OutputLimit:64})
 if err==nil||!r.TimedOut{t.Fatalf("%+v %v",r,err)}
}
func TestBoundedOutput(t *testing.T){
 if _,err:=os.Stat("/bin/echo");err!=nil{t.Skip("echo unavailable")}
 r,err:=Run(context.Background(),Spec{Name:"echo",Executable:"/bin/echo",Args:[]string{strings.Repeat("x",200)},Timeout:time.Second,OutputLimit:16})
 if err!=nil||!r.Truncated||len(r.Output)!=16{t.Fatalf("%+v %v",r,err)}
}
