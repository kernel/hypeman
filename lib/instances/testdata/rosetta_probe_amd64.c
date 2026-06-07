static long sys_write(long fd, const void *buf, unsigned long n){
  long r; __asm__ volatile("syscall":"=a"(r):"a"(1),"D"(fd),"S"(buf),"d"(n):"rcx","r11","memory"); return r;
}
__attribute__((noreturn)) static void sys_exit(long c){
  __asm__ volatile("syscall"::"a"(60),"D"(c)); __builtin_unreachable();
}
void _start(void){
  static const char m[]="ROSETTA_X86_OK\n";
  sys_write(1, m, sizeof(m)-1);
  sys_exit(0);
}
