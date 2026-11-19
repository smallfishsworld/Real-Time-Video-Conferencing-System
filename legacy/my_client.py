import multiprocessing
import threading
import subprocess
import queue
import time
from datetime import datetime
import pickle

from util import *
from tkinter import *
import socket
from config import *


class ConferenceClient(threading.Thread):
    def __init__(self):
        super().__init__()
        self.is_working = True
        self.server_addr = (SERVER_IP, MAIN_SERVER_PORT)
        self.on_meeting = False  # status
        self.is_meeting_creator = False
        self.conn = socket.socket(socket.AF_INET, socket.SOCK_STREAM)  # you may need to maintain multiple conns for a single conference
        self.conn.connect(self.server_addr)
        
        
        self.support_data_types = []  # for some types of data
        self.share_data = {}
        self.conference_id = -1

        self.video_open = True
        self.audio_open = True

        self.name = input('please input your name:')
        self.recv_data = None  # you may need to save received streamd data from other clients in conference
        print('start')
        
        self.ID = int(self.name)
        self.users = set()
        self.users_conns = {}
        
        self.p2p = True
        
        
    
        self.soc = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
        
        self.soc.setsockopt(socket.SOL_SOCKET, socket.SO_SNDBUF, 100000)
        
        self.soc.bind(("0.0.0.0",(self.ID*100)+20000))
        
    
        

    def create_conference(self):
        """
        create a conference: send create-conference request to server and obtain necessary data to
        """
        conference_id = input('input your conference_id:')
        self.conn.send('create'.rjust(10).encode())
        self.conn.send(self.name.rjust(10).encode())
        self.conn.send(conference_id.rjust(10).encode())

    def join_conference(self, conference_id):
        """
        join a conference: send join-conference request with given conference_id, and obtain necessary data to
        """
        self.conn.send('join'.rjust(10).encode())
        self.conn.send(self.name.rjust(10).encode())
        self.conn.send(conference_id.rjust(10).encode())

    def quit_conference(self):
        """
        quit your on-going conference
        """
        se='quit:'+str(self.ID)
        self.cfconn.send(se.rjust(10).encode())
        self.cfclear()
        self.is_meeting_creator=False
        
        print('finish!!!')
        print(" ")
        self.cmd_status[0]=0

    def cancel_conference(self):
        """
        cancel your on-going conference (when you are the conference manager): ask server to close all clients
        """
        self.cfconn.send('cancel'.rjust(10).encode())

    def keep_share(self):
        '''
        running task: keep sharing (capture and send) certain type of data from server or clients (P2P)
        you can create different functions for sharing various kinds of data
        '''
        try:
            while True:
                if self.on_meeting:
                    frm = capture_camera()
                    img_da = compress_image(frm)
                    aud_da = streamin.read(CHUNK)

                    if self.video_open:
                        img_tuple = pickle.dumps((self.ID, 1, img_da))

                        if self.p2p == False:
                            self.soc.sendto(img_tuple, self.cfdaconn)
                        else:
                            for nid in self.users.copy():
                                if nid !=0: 
                                    now_conn=(self.users_conns[nid],(nid*100)+20000)
                                    self.soc.sendto(img_tuple, now_conn)

                    if self.audio_open:
                        aud_tuple = pickle.dumps((self.ID, 2, aud_da))
                        
                        if self.p2p == False:
                            self.soc.sendto(aud_tuple, self.cfdaconn)
                        else:
                            for nid in self.users.copy():
                                if nid !=0: 
                                    now_conn=(self.users_conns[nid],(nid*100)+20000)
                                    self.soc.sendto(aud_tuple, now_conn)

                time.sleep(0.0001)


        except Exception as e:
            print(e)
            
            # print("P2P",self.p2p)
            
        
        
    def keep_recv(self):
        '''
        running task: keep receiving certain type of data (save or output)
        you can create other functions for receiving various kinds of data
        '''
        try:
            while True:
                if self.on_meeting:
                    data, _ = self.soc.recvfrom(100000)
                    rcv_tuple = pickle.loads(data)

                    if rcv_tuple[1] == 1:
                        img_da = decompress_image(rcv_tuple[2])
                        self.vid_da[rcv_tuple[0]]=img_da
                    elif rcv_tuple[1] == 2:
                        aud_da = rcv_tuple[2]
                        streamout.write(aud_da)
                else:
                    time.sleep(0.02)  
        except Exception as e:
            print(e)
    


    def videoshow(self):
        lis = self.users.copy() & self.vid_da.keys()
        for nid in lis:
            cv2.imshow(str(nid), np.array(self.vid_da[nid]))
            cv2.waitKey(1)


    def videoplayer(self):
        
        self.users.add(0)
        while True: 
            if self.on_meeting:
                self.vid_da[0] = capture_camera()
                self.videoshow()
            
            time.sleep(0.02) 
            
    
    
    def debug(self):
        while True:
            print("CHK!!!!")
            print(self.users)
            print(self.users_conns)
            print(self.p2p)
            time.sleep(10)
            
            
    def auto_mode_switch(self):
        while True: 
            if not self.on_meeting:
                time.sleep(0.3)  
                continue
            
            if len(self.users)>2:
                if(self.p2p==True):
                    self.p2p=False
                    subprocess.Popen(f'start cmd /k echo {"mode switches from p2p to u-s"}', shell=True)
            
            elif len(self.users)<=2:
                if(self.p2p==False):
                    self.p2p=True
                    subprocess.Popen(f'start cmd /k echo {"mode switches from u-s to p2p"}', shell=True)

            time.sleep(0.3)
        
    
    def cfclear(self):
        self.on_meeting = False
        self.cfconn.close()
        self.users.clear()
        self.users.add(0)

    def share_switch(self, data_type):
        '''
        switch for sharing certain type of data (screen, camera, audio, etc.)
        '''
        pass

    

    def output_data(self):
        '''
        running task: output received stream data
        '''

    def start_conference(self):
        '''
        init conns when create or join a conference with necessary conference_info
        and
        start necessary running task for conference
        '''
        
        
        self.cfconn = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        self.cfconn.bind(("0.0.0.0",self.ID+20000))
        
        self.cfconn.connect((SERVER_IP,self.conference_id+10000))
        
        
        
        self.on_meeting=True
        
        self.vid_da = {}
        
        self.cfdaconn = (SERVER_IP,(self.conference_id*100)+10000)
        
        thd11=threading.Thread(target=self.keep_share)
        thd11.start()

        thd12=threading.Thread(target=self.keep_recv)
        thd12.start()

        thd13=threading.Thread(target=self.videoplayer)
        thd13.start()

        thd14=threading.Thread(target=self.auto_mode_switch)
        thd14.start()

        thd15=threading.Thread(target=self.debug)
        thd15.start()


        
        
        

    def close_conference(self):
        '''
        close all conns to servers or other clients and cancel the running tasks
        pay attention to the exception handling
        '''

    def conf_sending(self,send_taskq):
        a=1
        while True:
            if not self.on_meeting:
                time.sleep(0.02) 
                continue
            try:
                cmd_input=send_taskq.get()
                self.cfconn.send((cmd_input + '(' + datetime.now().strftime('%Y-%m-%d %H:%M:%S') + ')').rjust(10).encode())
            except Exception() as e:
                print(e)
            time.sleep(0.02)


    def conf_listening(self):
        while True:
            if self.on_meeting:
                try:
                    message = self.cfconn.recv(1024).decode().strip()
                    if not message:
                        continue
                    
                    cmd=message.split(":")
                    print("MMMM",message)
                    
                    if cmd[0] == "joinlist":
                        for i in range(len(cmd)-2):
                            dt=cmd[i+1].split("#")
                            
                            self.users.add(int(dt[0]))
                            self.users_conns[int(dt[0])]=dt[1]
                            
                    if len(cmd)==1:
                        if message == "exit":
                            # self.on_meeting = False
                            self.cfclear()
                            self.is_meeting_creator = False
                            print("You have quitted the conference because the creator cancelled it!!!")
                            self.cmd_status[0] = 0
                    elif len(cmd)==2:
                        if cmd[0] == "join":
                            
                            dt=cmd[1].split("#")
                            cid=int(dt[0])
                            self.users.add(cid)
                            self.users_conns[cid]=dt[1]
                            
                            
                        elif cmd[0] == "quit":
                            cid=int(cmd[1])
                            self.users.discard(cid)
                        
                        elif cmd[0] == "send":
                            subprocess.Popen(f'start cmd /k echo {"receive from conference:"+cmd[1]}', shell=True)
                        
                    else:
                        subprocess.Popen(f'start cmd /k echo {"receive from conference:"+message}', shell=True)
                
                except Exception() as e:
                    print(e)
        

    def copy_with_received_info(self):
        return self.recv_data.strip().split(' ')





    def sending(self,cmd_taskq):
        while True:
            if not self.on_meeting:
                status = 'Free'
            else:
                status = f'OnMeeting-{self.conference_id}'

            recognized = True
            
           
            cmd_input=cmd_taskq.get()

            fields = cmd_input.split(maxsplit=1)
            
            
            print("CMDTST",fields)
            if len(fields) == 1:
                if cmd_input in ('?', '？'):
                    print(HELP)
                    print('finish!!!')
                    print(" ")
                    self.cmd_status[0]=0
                elif cmd_input == 'create':
                    if self.on_meeting == True:
                        print("Not allowed to create a new meeting when already in a meeting!")
                        print('finish!!!')
                        print(" ")
                        self.cmd_status[0]=0
                    else:
                        self.create_conference()
                elif cmd_input == 'quit':
                    if self.on_meeting == False:
                        print("Not allowed to quit a meeting when not in a meeting!")
                        print('finish!!!')
                        print(" ")
                        self.cmd_status[0]=0
                    else:
                        self.quit_conference()
                elif cmd_input == 'cancel':
                    if self.on_meeting == False:
                        print("Not allowed to cancel a meeting when not in a meeting!")
                        print('finish!!!')
                        print(" ")
                        self.cmd_status[0]=0
                    elif self.is_meeting_creator == False:
                        print("Not allowed to cancel a meeting because you are not the meeting owner!")
                        print('finish!!!')
                        print(" ")
                        self.cmd_status[0]=0
                    else:
                        self.cancel_conference()
                elif cmd_input == "chgmode":
                    if self.p2p == False:
                        self.p2p = True 
                    else:
                        self.p2p = False 
                        
                    self.cmd_status[0]=0
                    
                else:
                    recognized = False
            elif len(fields) == 2:
                if fields[0] == 'join':
                    if self.on_meeting == True:
                        print("Not allowed to join a meeting when already in a meeting!")
                        print('finish!!!')
                        print(" ")
                        self.cmd_status[0]=0
                    else:
                        input_conf_id = fields[1]
                        if input_conf_id.isdigit():
                            self.join_conference(input_conf_id)
                        else:
                            print('[Warn]: Input conference ID must be in digital form')
                elif fields[0] == 'switch':
                    data_type = fields[1]
                    if data_type == "audio":
                        if self.audio_open == True:
                            self.audio_open=False
                        else:
                            self.audio_open=True
                    
                    if data_type == "video":
                        if self.video_open == True:
                            self.video_open=False
                        else:
                            self.video_open=True
                            
                    self.cmd_status[0]=0


                else:
                    recognized = False
            else:
                recognized = False
            
            if not recognized:
                print(f'[Warn]: Unrecognized cmd_input {cmd_input}')
                print('finish!!!')
                print(" ")
                self.cmd_status[0]=0

    def listening(self):
        
        while True:
            message = self.conn.recv(1024).decode().strip()
            if not message:
                continue
            subprocess.Popen(f'start cmd /k echo {message}', shell=True)
            infos = message.split(':')
            print("U ",infos)

            
            if infos[0]=="200":
                if infos[1]=="join":
                    print("CHK: success join"+infos[2])
                    self.conference_id=int(infos[2])

                    self.start_conference()
                    self.on_meeting=True
                    self.is_meeting_creator=False
                elif infos[1]=="create":
                    print("CHK: success join"+infos[2])
                    self.conference_id=int(infos[2])

                    self.start_conference()
                    self.on_meeting=True
                    self.is_meeting_creator=True


            print('finish!!!')
            print(" ")
            self.cmd_status[0]=0


    def run(self):
        
        threads = []
        self.cmd_taskq=queue.Queue()
        self.send_taskq=queue.Queue()

        self.cmd_status=[0]
        thd1=threading.Thread(target=self.sending,args=(self.cmd_taskq,))
        thd1.start()
        threads.append(thd1)
        thd2=threading.Thread(target=self.listening)
        thd2.start()
        threads.append(thd2)
        thd3=threading.Thread(target=self.conf_sending,args=(self.send_taskq,))
        thd3.start()
        threads.append(thd3)
        thd4=threading.Thread(target=self.conf_listening)
        thd4.start()
        threads.append(thd4)

        while True:
            if not self.on_meeting:
                status = 'Free'
            else:
                status = f'OnMeeting-{self.conference_id}'

            if not self.on_meeting: 
                print(f'({status}) Please enter a operation (enter "?" to help)')
            else:
                print(f'({status}) Please enter a operation (enter "?" to help), or enter "send:[message]" to send')
            
            ipt=input()
            fields=ipt.split(":")
            if(self.on_meeting and len(fields)>=2 and fields[0]=="send"):
                self.send_taskq.put(ipt)
            else:
                self.cmd_status[0]=1
                self.cmd_taskq.put(ipt)
                
                
                while self.cmd_status[0]==1:
                    a=1


if __name__ == '__main__':
    client = ConferenceClient()
    client.start()
    a=1
    while True:
        a=1
