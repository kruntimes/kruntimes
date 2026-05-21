kruntime 是一个运行在Kubernetes之上的双层调度系统，通过保持预热运行时(Runtime pod) 在毫秒内准备好执行代码，消除冷启动延迟。

核心的用户体验
1. 用户接口
   - 通过Kubernetes API进行交互，用户可以使用kubectl命令行工具创建和管理Run CRD对象来执行代码。
   - 通过SDK(python, golang等)在kruntime上运行代码。
 