# 使用官方Python运行时作为父镜像
FROM python:3.10

# 设置工作目录为/app
WORKDIR /app

# 将当前目录内容复制到位于/app中的容器中
COPY . /app

# 安装requirements.txt中指定的任何所需包
# 假设你的应用有一个requirements.txt文件
RUN pip install --no-cache-dir -r requirements.txt

# 在容器启动时运行你的应用，例如app.py
CMD ["python", "./app.py"]
