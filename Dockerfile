FROM centos
COPY main /webhook

CMD ["/webhook"]
